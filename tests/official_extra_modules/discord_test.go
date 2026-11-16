package official_extra_modules

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
)

// discordWebhookToken is the secret part of the address of a Discord
// webhook, after its number.
const discordWebhookToken = "Ab1-Cd2_Ef3SECRETsecretSECRETghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOP"

// discordWebhookPath is the path of the address that Discord shows for a
// webhook, which names no version of the API.
const discordWebhookPath = "/api/webhooks/123456789012345678/" + discordWebhookToken

// discordExecutePath is the path to which the module posts: the webhook's,
// with version 10 of the API.
const discordExecutePath = "/api/v10/webhooks/123456789012345678/" + discordWebhookToken

// discordRequest is one request that the stand-in for Discord got. host is
// the host that the request names, which a proxy gets in place of its own.
type discordRequest struct {
	method, host, path, query, contentType, userAgent string
	body                                              []byte
}

// discordService stands for Discord's API. answer decides what it answers
// to each request.
type discordService struct {
	*httptest.Server
	mu       sync.Mutex
	requests []discordRequest
}

func startDiscord(t *testing.T, answer func(discordRequest) (status int, body string)) *discordService {
	t.Helper()
	s := &discordService{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		request := discordRequest{r.Method, r.Host, r.URL.Path, r.URL.RawQuery, r.Header.Get("Content-Type"), r.Header.Get("User-Agent"), body}
		s.mu.Lock()
		s.requests = append(s.requests, request)
		s.mu.Unlock()
		status, text := answer(request)
		// A redirect names another address of the same service, which a
		// module must not follow.
		if status/100 == 3 {
			w.Header().Set("Location", "/moved"+r.URL.Path)
		}
		w.WriteHeader(status)
		io.WriteString(w, text)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *discordService) got() []discordRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]discordRequest{}, s.requests...)
}

// discordMessage reads a request to Execute Webhook.
func discordMessage(t *testing.T, request discordRequest) map[string]any {
	t.Helper()
	if request.method != "POST" || request.contentType != "application/json" || request.userAgent != "MMWatchdog discord module" {
		t.Fatalf("not a JSON POST of the module: %+v", request)
	}
	var message map[string]any
	decoder := json.NewDecoder(bytes.NewReader(request.body))
	decoder.UseNumber()
	if e := decoder.Decode(&message); e != nil {
		t.Fatal(e, string(request.body))
	}
	return message
}

// discordTooLong is Discord's answer to a content of more than 2,000
// characters, with the form error of version 10 of the API.
const discordTooLong = `{"message": "Invalid Form Body", "code": 50035, "errors": {"content": {"_errors": [{"code": "BASE_TYPE_MAX_LENGTH", "message": "Must be 2000 or fewer in length."}]}}}`

// discord answers as Discord does with wait=true: it takes a content of at
// most 2,000 characters, as counted by countCharacters, answers with the
// message that it saved, and refuses a longer content.
func discord(t *testing.T, countCharacters func(string) int) *discordService {
	return startDiscord(t, func(request discordRequest) (int, string) {
		var message struct {
			Content string `json:"content"`
		}
		json.Unmarshal(request.body, &message)
		if countCharacters(message.Content) > 2000 {
			return 400, discordTooLong
		}
		saved, _ := json.Marshal(message.Content)
		return 200, `{"id": "1290000000000000001", "type": 0, "content": ` + string(saved) + `, "channel_id": "1210000000000000002", "webhook_id": "123456789012345678", "flags": 4, "mentions": [], "mention_roles": [], "mention_everyone": false}`
	})
}

// The discord module posts one message to the webhook in its file, for
// every kind of alert, and tells MMWatchdog that Discord saved it: the title with
// the alert's severity in front as MMWatchdog wrote it, after the mention if there
// is one, and below it the body in a code block, which Discord shows as it
// is. Only the mention notifies anybody.
func TestDiscordSendsTheTitleAndTheBody(t *testing.T) {
	t.Parallel()
	server := discord(t, utf8.RuneCountInString)
	recovery := failure
	recovery.Type, recovery.Title, recovery.TitleWithAlertSeverity, recovery.Body = "recovered", "app-1: myapp/http is ok again", "Resolved SOS: app-1: myapp/http is ok again", "HTTP 200"
	reminder := failure
	reminder.Type, reminder.Title, reminder.TitleWithAlertSeverity, reminder.Body = "reminder", "app-1: myapp/http still failing", "SOS: app-1: myapp/http still failing", "myapp/http still failing since 14:02 UTC; latest result: expected HTTP 200, received 503"
	grouped := failure
	grouped.ID, grouped.Node, grouped.Title, grouped.TitleWithAlertSeverity, grouped.Body = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "", "myapp/http failed on 2 nodes", "SOS: myapp/http failed on 2 nodes", "myapp/http failed on 2 nodes: app-1, app-2\nfrom app-1:\n  expected HTTP 200, received 503\n  failing since 14:02 UTC"
	notice := failure
	notice.Type, notice.AlertSeverity, notice.Incident, notice.CheckGroup, notice.Check = "notice", "info", "", "", ""
	notice.Title, notice.TitleWithAlertSeverity, notice.Body, notice.Message = "app-1: Test of the team chat. Nothing is wrong.", "Info: app-1: Test of the team chat. Nothing is wrong.", "", ""
	onlyTheTitle := failure
	onlyTheTitle.Body, onlyTheTitle.Message = "", ""
	odd := failure
	odd.Title, odd.TitleWithAlertSeverity, odd.Body = "app-1: "+awkward[:22], "SOS: app-1: "+awkward[:22], awkward
	// Outside the code block, Discord reads formatting and mentions, so
	// the first line gets a backslash in front of each such character.
	inTitle := failure
	inTitle.Type, inTitle.AlertSeverity, inTitle.Body = "notice", "info", "HTTP 200"
	inTitle.TitleWithAlertSeverity = "Info: app-1: restarted *all* __workers__ ~~now~~ `x` ||y|| [z](https://example.com) <@999> <#1> <t:1> C:\\temp @everyone @here a-b # c -# d > e"
	// Three backticks would end the code block, so they arrive as three
	// single quotes; fewer stay.
	fenced := failure
	fenced.Body = "the output was:\n" + codeBlock + "\nstarted `myapp` with ``two``\n" + codeBlock + codeBlock + "`"
	for _, tc := range []struct {
		name            string
		alert           checksandalerts.Event
		mention         string
		want            string
		allowedMentions string
	}{
		{"a failure", failure, "", "SOS: app-1: myapp/http failed\n" + codeBlock + "\nexpected HTTP 200, received 503\nfailing since 14:02 UTC\n" + codeBlock, `{"parse":[]}`},
		{"a failure with a person's mention", failure, "<@123456789012345678>", "<@123456789012345678> SOS: app-1: myapp/http failed\n" + codeBlock + "\nexpected HTTP 200, received 503\nfailing since 14:02 UTC\n" + codeBlock, `{"parse":[],"users":["123456789012345678"]}`},
		{"a failure with a role's mention", failure, "<@&165511591545143296>", "<@&165511591545143296> SOS: app-1: myapp/http failed\n" + codeBlock + "\nexpected HTTP 200, received 503\nfailing since 14:02 UTC\n" + codeBlock, `{"parse":[],"roles":["165511591545143296"]}`},
		{"a failure that mentions everyone", failure, "@everyone", "@everyone SOS: app-1: myapp/http failed\n" + codeBlock + "\nexpected HTTP 200, received 503\nfailing since 14:02 UTC\n" + codeBlock, `{"parse":["everyone"]}`},
		{"a recovery", recovery, "", "Resolved SOS: app-1: myapp/http is ok again\n" + codeBlock + "\nHTTP 200\n" + codeBlock, `{"parse":[]}`},
		{"a reminder", reminder, "", "SOS: app-1: myapp/http still failing\n" + codeBlock + "\nmyapp/http still failing since 14:02 UTC; latest result: expected HTTP 200, received 503\n" + codeBlock, `{"parse":[]}`},
		{"a grouped message of the hub", grouped, "", "SOS: myapp/http failed on 2 nodes\n" + codeBlock + "\nmyapp/http failed on 2 nodes: app-1, app-2\nfrom app-1:\n  expected HTTP 200, received 503\n  failing since 14:02 UTC\n" + codeBlock, `{"parse":[]}`},
		// Without a body there is no code block.
		{"a notice without a body", notice, "", "Info: app-1: Test of the team chat. Nothing is wrong.", `{"parse":[]}`},
		{"only the title", onlyTheTitle, "@here", "@here SOS: app-1: myapp/http failed", `{"parse":["everyone"]}`},
		// The code block shows the body as it is: quotes, backslashes, <, >,
		// &, formatting characters, and characters outside ASCII.
		{"awkward text", odd, "", "SOS: app-1: it said \"no\" & 'never'\n" + codeBlock + "\n" + awkward + "\n" + codeBlock, `{"parse":[]}`},
		{"formatting characters and mentions in the title", inTitle, "<@123456789012345678>",
			"<@123456789012345678> Info: app-1: restarted \\*all\\* \\_\\_workers\\_\\_ \\~\\~now\\~\\~ \\`x\\` \\|\\|y\\|\\| \\[z](https://example.com) \\<@999\\> \\<#1\\> \\<t:1\\> C:\\\\temp @" + zeroWidthSpace + "everyone @" + zeroWidthSpace + "here a-b # c -# d \\> e\n" + codeBlock + "\nHTTP 200\n" + codeBlock,
			`{"parse":[],"users":["123456789012345678"]}`},
		{"three backticks in the body", fenced, "", "SOS: app-1: myapp/http failed\n" + codeBlock + "\nthe output was:\n'''\nstarted `myapp` with ``two``\n''''''`\n" + codeBlock, `{"parse":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(server.got())
			inputs := map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/discord"}
			if tc.mention != "" {
				inputs["mention"] = tc.mention
			}
			o := run(t, "discord", tc.alert, inputs, map[string]string{"webhook_url_file": server.URL + discordWebhookPath})
			if o.exitStatus != 0 || o.status != "delivered" || o.message != "Discord accepted the message" || o.stderr != "" {
				t.Fatalf("%+v", o)
			}
			requests := server.got()[before:]
			if len(requests) != 1 || requests[0].path != discordExecutePath || requests[0].query != "wait=true" {
				t.Fatalf("%+v", requests)
			}
			message := discordMessage(t, requests[0])
			allowedMentions, _ := json.Marshal(message["allowed_mentions"])
			if message["content"] != tc.want || string(allowedMentions) != tc.allowedMentions || message["flags"] != json.Number("4") || len(message) != 3 {
				t.Fatalf("got  %q\nwant %q\nmessage %v", message["content"], tc.want, message)
			}
			if o.holds(discordWebhookToken) {
				t.Fatalf("the address of the webhook is in what the module wrote: %+v", o)
			}
		})
	}
}

// The module posts to version 10 of Discord's API, and asks Discord to
// answer with the saved message, whatever form of the webhook's address the
// file holds.
func TestDiscordAsksForVersion10AndForTheSavedMessage(t *testing.T) {
	t.Parallel()
	server := discord(t, utf8.RuneCountInString)
	for _, tc := range []struct{ address, path, query string }{
		{discordWebhookPath, discordExecutePath, "wait=true"},
		{discordExecutePath, discordExecutePath, "wait=true"},
		{discordWebhookPath + "?thread_id=1300000000000000003", discordExecutePath, "thread_id=1300000000000000003&wait=true"},
	} {
		o := run(t, "discord", failure, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/discord"}, map[string]string{"webhook_url_file": server.URL + tc.address})
		requests := server.got()
		if o.status != "delivered" || requests[len(requests)-1].path != tc.path || requests[len(requests)-1].query != tc.query {
			t.Fatalf("%s: %+v %+v", tc.address, o, requests[len(requests)-1])
		}
	}
}

// The hosts of Discord's former name, discordapp.com, refuse version 10 of
// the API with "Invalid API version" (code 50041), so the module posts to
// the same host of today's name, discord.com. Every other host stays. The
// request goes to a proxy of the test, which sees the host that the module
// asked for, so no name is looked up.
func TestDiscordAsksDiscordComInPlaceOfDiscordapp(t *testing.T) {
	t.Parallel()
	proxy := discord(t, utf8.RuneCountInString)
	for host, want := range map[string]string{
		"discordapp.com":        "discord.com",
		"ptb.discordapp.com":    "ptb.discord.com",
		"canary.discordapp.com": "canary.discord.com",
		"discord.com":           "discord.com",
		"ptb.discord.com":       "ptb.discord.com",
		"chat.example.com":      "chat.example.com",
		"chat.example.com:8443": "chat.example.com:8443",
	} {
		before := len(proxy.got())
		o := run(t, "discord", failure, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/discord"}, map[string]string{"webhook_url_file": "http://" + host + discordWebhookPath}, "HTTP_PROXY="+proxy.URL)
		requests := proxy.got()[before:]
		if o.status != "delivered" || len(requests) != 1 || requests[0].host != want || requests[0].path != discordExecutePath || requests[0].query != "wait=true" {
			t.Fatalf("%s: %+v %+v", host, o, requests)
		}
	}
}

// discordPings returns whom Discord would notify for a message, the
// person, the role, or the members, by the mentions that its content holds
// and its allowed_mentions. It reads the content as a careless reader
// would: inside a code block, and after a backslash too.
func discordPings(t *testing.T, request discordRequest) []string {
	t.Helper()
	var message struct {
		Content         string `json:"content"`
		AllowedMentions struct {
			Parse []string `json:"parse"`
			Users []string `json:"users"`
			Roles []string `json:"roles"`
		} `json:"allowed_mentions"`
	}
	if e := json.Unmarshal(request.body, &message); e != nil {
		t.Fatal(e)
	}
	// Discord notifies each person, role, or group of members once,
	// however often the content mentions it.
	pings := []string{}
	ping := func(whom string) {
		if !slices.Contains(pings, whom) {
			pings = append(pings, whom)
		}
	}
	for _, found := range regexp.MustCompile(`<@(&?)!?([0-9]+)>|@(everyone|here)`).FindAllStringSubmatch(message.Content, -1) {
		switch {
		case found[3] != "" && slices.Contains(message.AllowedMentions.Parse, "everyone"):
			ping("@" + found[3])
		case found[1] == "&" && (slices.Contains(message.AllowedMentions.Parse, "roles") || slices.Contains(message.AllowedMentions.Roles, found[2])):
			ping("role " + found[2])
		case found[1] == "" && found[3] == "" && (slices.Contains(message.AllowedMentions.Parse, "users") || slices.Contains(message.AllowedMentions.Users, found[2])):
			ping("person " + found[2])
		}
	}
	return pings
}

// Discord notifies the one person, role, or group of members that the
// destination's mention names, and nobody that the text of an alert names:
// not an @everyone or a <@123> in the output of a check, and not one in the
// title of a notice.
func TestDiscordNotifiesOnlyTheMentionOfTheDestination(t *testing.T) {
	t.Parallel()
	server := discord(t, utf8.RuneCountInString)
	loud := failure
	loud.TitleWithAlertSeverity = "SOS: app-1: @everyone <@111> <@&222> @here failed"
	loud.Body = "@everyone look\n<@111> <@!111> <@&222>\n@here\n\\@everyone `@everyone` " + codeBlock + "\n@everyone"
	for _, tc := range []struct {
		mention string
		pings   []string
	}{
		{"", []string{}},
		{"<@123456789012345678>", []string{"person 123456789012345678"}},
		{"<@111>", []string{"person 111"}},
		{"<@&165511591545143296>", []string{"role 165511591545143296"}},
		{"@everyone", []string{"@everyone"}},
		{"@here", []string{"@here"}},
	} {
		before := len(server.got())
		inputs := map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/discord"}
		if tc.mention != "" {
			inputs["mention"] = tc.mention
		}
		o := run(t, "discord", loud, inputs, map[string]string{"webhook_url_file": server.URL + discordWebhookPath})
		requests := server.got()[before:]
		if o.status != "delivered" || len(requests) != 1 {
			t.Fatalf("%q: %+v", tc.mention, o)
		}
		if pings := discordPings(t, requests[0]); !reflect.DeepEqual(pings, tc.pings) {
			t.Errorf("the mention %q: Discord would notify %v, and the destination asks for %v; content %q", tc.mention, pings, tc.pings, discordMessage(t, requests[0])["content"])
		}
	}
}

// A message of more than 2,000 characters is cut to at most 2,000: the body
// is cut between two characters, the cut is marked, and the first line and
// the end of the code block always arrive. When Discord still refuses the
// content as too long, because it counts some characters as two, the
// module sends 1,000 characters.
func TestDiscordCutsALongText(t *testing.T) {
	t.Parallel()
	utf16Length := func(text string) int { return len(utf16.Encode([]rune(text))) }
	start := "<@&165511591545143296> SOS: app-1: my\\_app/http failed\n" + codeBlock + "\n"
	end := "\n" + codeBlock
	for _, tc := range []struct {
		name            string
		body            string
		countCharacters func(string) int
		requests, want  int
	}{
		{"characters that Discord counts once", strings.Repeat("ü🐒& ", 1000), utf8.RuneCountInString, 1, 2000},
		{"characters that Discord counts twice", strings.Repeat("🐒", 3000), utf16Length, 2, 1000},
		{"a text of exactly the limit", strings.Repeat("x", 2000-utf8.RuneCountInString(start+end)), utf8.RuneCountInString, 1, 2000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := discord(t, tc.countCharacters)
			long := failure
			long.CheckGroup, long.TitleWithAlertSeverity, long.Body = "my_app", "SOS: app-1: my_app/http failed", tc.body
			inputs := map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/discord", "mention": "<@&165511591545143296>"}
			o := run(t, "discord", long, inputs, map[string]string{"webhook_url_file": server.URL + discordWebhookPath})
			requests := server.got()
			if o.status != "delivered" || len(requests) != tc.requests {
				t.Fatalf("%d requests; %+v", len(requests), o)
			}
			content, _ := discordMessage(t, requests[len(requests)-1])["content"].(string)
			sent := strings.TrimSuffix(strings.TrimPrefix(content, start), end)
			cut := utf8.RuneCountInString(start+tc.body+end) > tc.want
			if !utf8.ValidString(content) || utf8.RuneCountInString(content) != tc.want || !strings.HasPrefix(content, start) || !strings.HasSuffix(content, end) ||
				cut != strings.HasSuffix(sent, "…") || !strings.HasPrefix(tc.body, strings.TrimSuffix(sent, "…")) {
				t.Fatalf("%d characters, cut %v, ends with %q", utf8.RuneCountInString(content), cut, content[len(content)-12:])
			}
		})
	}
}

// Every answer other than the message that Discord saved is a failed
// attempt with a message that a person can act on, and no failure shows the
// address of the webhook.
func TestDiscordFailures(t *testing.T) {
	t.Parallel()
	const webhookAdvice = " The webhook was deleted, or the address in the file is wrong. Write the address of a webhook from Discord into the file that webhook_url_file names."
	const blockAdvice = " Cloudflare, in front of Discord, answers so while it blocks this node's network address, for a time after many requests."
	const faultAdvice = " Discord, or Cloudflare in front of it, had a fault. MMWatchdog tries again."
	const addressAdvice = " Check the address in the file that webhook_url_file names, and any proxy between this node and Discord."
	// A body can repeat the request: {request} stands for its path and its
	// query, and {address} for the whole address, as a server that is not
	// Discord can write them.
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"a webhook that was deleted", 404, `{"message": "Unknown Webhook", "code": 10015}`, "Discord answered 404: Unknown Webhook (code 10015)." + webhookAdvice},
		{"a wrong token", 401, `{"message": "Invalid Webhook Token", "code": 50027}`, "Discord answered 401: Invalid Webhook Token (code 50027)." + webhookAdvice},
		{"a webhook of a forum channel", 400, `{"message": "Webhooks posted to forum channels must have a thread_name or thread_id", "code": 220001}`,
			"Discord answered 400: Webhooks posted to forum channels must have a thread_name or thread_id (code 220001). The webhook is in a forum channel, where a message needs a thread. Make a webhook for a text channel."},
		{"a rate limit", 429, `{"message": "You are being rate limited.", "retry_after": 0.376, "global": false}`,
			"Discord answered 429: You are being rate limited. Discord takes no more messages from this webhook for 1 second."},
		{"the global rate limit", 429, `{"message": "You are being rate limited.", "retry_after": 64.57, "global": true}`,
			"Discord answered 429: You are being rate limited. Discord takes no requests from this node's network address for 65 seconds. The limit counts the requests of every program there."},
		{"a rate limit without a time", 429, `{"message": "The resource is being rate limited.", "global": false}`,
			"Discord answered 429: The resource is being rate limited. Discord takes no more messages from this webhook for a short time."},
		{"a block of Cloudflare in Discord's JSON", 403, `{"message": "internal network error", "code": 40333}`,
			"Discord answered 403: internal network error (code 40333). Cloudflare, in front of Discord, is blocking the request; Discord says that a proper User-Agent often resolves this. The module sends one, so check that no proxy on the way changes it."},
		{"a block of Cloudflare as a page", 429, "<!DOCTYPE html>\n<html>\n<head>\n<title>Access denied | discord.com used\n  Cloudflare to restrict access</title>\n</head>\n<body>Error 1015</body>\n</html>",
			"the server at the webhook's address answered 429 without Discord's JSON: Access denied | discord.com used Cloudflare to restrict access." + blockAdvice},
		{"a block of Cloudflare as text", 403, "error code: 1020\n", "the server at the webhook's address answered 403 without Discord's JSON: error code: 1020." + blockAdvice},
		{"another refusal", 400, `{"message": "Cannot send an empty message", "code": 50006}`, "Discord answered 400: Cannot send an empty message (code 50006)."},
		{"an error of Discord", 500, `{"message": "500: Internal Server Error", "code": 0}`, "Discord answered 500: 500: Internal Server Error (code 0). Discord had a fault. MMWatchdog tries again."},
		{"a fault page of Cloudflare", 502, "<!DOCTYPE html>\n<html><head><title>discord.com | 502: Bad gateway</title></head><body>Cloudflare</body></html>",
			"the server at the webhook's address answered 502 without Discord's JSON: discord.com | 502: Bad gateway." + faultAdvice},
		{"a fault page of another server", 504, "<html><head><title>504 Gateway Time-out</title></head><body>nginx</body></html>",
			"the server at the webhook's address answered 504 without Discord's JSON: 504 Gateway Time-out." + faultAdvice},
		{"a page of another server", 404, "<html><head><title>404 Not Found</title></head><body>nginx</body></html>",
			"the server at the webhook's address answered 404 without Discord's JSON: 404 Not Found." + addressAdvice},
		{"JSON of another server, whose message is no text", 400, `{"message": {"text": "no"}}`,
			"the server at the webhook's address answered 400 without Discord's JSON: {\"message\": {\"text\": \"no\"}}." + addressAdvice},
		{"an answer without the saved message", 204, "", "the server at the webhook's address answered 204 without Discord's JSON: no text." + addressAdvice},
		{"a redirect, which the module does not follow", 302, "", "the server at the webhook's address answered 302 without Discord's JSON: no text." + addressAdvice},
		// A server that is not Discord can repeat the address or its path,
		// and the module passes on neither the address nor the token.
		{"JSON of another server that repeats the path", 404, `{"message": "Route POST:{request} not found", "error": "Not Found", "statusCode": 404}`,
			"Discord answered 404: Route POST:/api/v10/webhooks/123456789012345678/[token of the webhook]?wait=true not found." + webhookAdvice},
		{"JSON of another server that repeats the address", 400, `{"message": "no webhook at {address}"}`,
			"Discord answered 400: no webhook at [address of the webhook]."},
		{"a page of another server that repeats the address", 405, "<html><head><title>Cannot POST {address}</title></head></html>",
			"the server at the webhook's address answered 405 without Discord's JSON: Cannot POST [address of the webhook]." + addressAdvice},
		{"a text of another server that repeats the path", 403, "forbidden: {request}",
			"the server at the webhook's address answered 403 without Discord's JSON: forbidden: /api/v10/webhooks/123456789012345678/[token of the webhook]?wait=true." + blockAdvice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := startDiscord(t, func(r discordRequest) (int, string) {
				return tc.status, strings.NewReplacer("{request}", r.path+"?"+r.query, "{address}", "http://"+r.host+r.path+"?"+r.query).Replace(tc.body)
			})
			o := run(t, "discord", failure, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/discord"}, map[string]string{"webhook_url_file": server.URL + discordWebhookPath})
			if len(server.got()) != 1 || o.holds(discordWebhookToken) {
				t.Fatalf("%d requests, or the webhook's token in what the module wrote: %+v", len(server.got()), o)
			}
			if o.exitStatus != 0 || o.status != "failed" || o.message != tc.want || len(o.message) > 300 {
				t.Fatalf("got  %q\nwant %q\n%d bytes; %+v", o.message, tc.want, len(o.message), o)
			}
		})
	}
	// MMWatchdog shows at most 300 bytes of a module's message. Long words of an
	// answer are cut, between two characters, so that the advice arrives
	// whole.
	for _, tc := range []struct {
		name, body, advice string
		status             int
	}{
		{"long words of Discord", `{"message": "` + strings.Repeat("Unknown Webhook ", 30) + `", "code": 10015}`, webhookAdvice, 404},
		{"long words outside ASCII", `{"message": "` + strings.Repeat("Știință ", 60) + `"}`, webhookAdvice, 401},
		{"a page with a long title", "<title>" + strings.Repeat("Access denied ", 40) + "</title>", blockAdvice, 429},
		{"a long fault page", "<title>" + strings.Repeat("Bad gateway ", 40) + "</title>", faultAdvice, 502},
		{"a long first line", strings.Repeat("not found ", 60), addressAdvice, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := startDiscord(t, func(discordRequest) (int, string) { return tc.status, tc.body })
			o := run(t, "discord", failure, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/discord"}, map[string]string{"webhook_url_file": server.URL + discordWebhookPath})
			if o.status != "failed" || len(o.message) > 300 || len(o.message) < 290 || !utf8.ValidString(o.message) || !strings.HasSuffix(o.message, "…."+tc.advice) {
				t.Fatalf("%d bytes: %q", len(o.message), o.message)
			}
		})
	}
	// A content that Discord refuses as too long also at 1,000 characters
	// fails after two requests, with Discord's words.
	t.Run("a content that is always too long", func(t *testing.T) {
		server := startDiscord(t, func(discordRequest) (int, string) { return 400, discordTooLong })
		o := run(t, "discord", failure, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/discord"}, map[string]string{"webhook_url_file": server.URL + discordWebhookPath})
		if want := "Discord answered 400: Invalid Form Body (code 50035): content: Must be 2000 or fewer in length."; o.status != "failed" || o.message != want || len(server.got()) != 2 {
			t.Fatalf("got  %q\nwant %q\n%d requests", o.message, want, len(server.got()))
		}
	})
	t.Run("nothing listens at the address", func(t *testing.T) {
		listener, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		address := "http://" + listener.Addr().String()
		listener.Close()
		o := run(t, "discord", failure, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/discord"}, map[string]string{"webhook_url_file": address + discordWebhookPath})
		if o.exitStatus != 0 || o.status != "failed" || !strings.HasPrefix(o.message, "MMWatchdog could not send the message to Discord: ") || !strings.Contains(o.message, "connection refused") || o.holds(discordWebhookToken) || o.holds("/webhooks/") {
			t.Fatalf("%+v", o)
		}
	})
	// A file that holds no webhook address fails before a request is sent.
	server := discord(t, utf8.RuneCountInString)
	const wrongFile = "the file that webhook_url_file names must hold the address of one Discord webhook, which looks like https://discord.com/api/webhooks/123456789012345678/AbC-dEf_123, and nothing else"

	for _, tc := range []struct {
		name, content, want string
	}{
		{"an address without its scheme", strings.TrimPrefix(server.URL, "http://") + discordWebhookPath, wrongFile},
		{"two lines", server.URL + discordWebhookPath + "\n" + server.URL + discordWebhookPath, wrongFile},
		{"two addresses", server.URL + discordWebhookPath + " " + server.URL + discordWebhookPath, wrongFile},
		{"an address with a quote", server.URL + "/api/webhooks/1/x\" --output /tmp/x \"", wrongFile},
		{"the address of a Slack webhook", server.URL + "/services/" + slackSecret, wrongFile},
		{"another scheme", "ftp://discord.example/api/webhooks/1/" + discordWebhookToken, wrongFile},
		{"an empty file", "", wrongFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inputs := map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/discord"}
			o := run(t, "discord", failure, inputs, map[string]string{"webhook_url_file": tc.content})
			if o.exitStatus != 0 || o.status != "failed" || o.message != tc.want || len(o.message) > 300 || o.holds(discordWebhookToken) {
				t.Fatalf("%+v", o)
			}
		})
	}
	if requests := server.got(); len(requests) != 0 {
		t.Fatalf("the module sent a request to an invalid webhook address: %+v", requests)
	}
}

// MMWatchdog checks what a destination states for discord against the module's
// description and validate_inputs, before the module runs. Secret file
// contents are checked at delivery.
func TestTheInputsOfDiscordAreValidated(t *testing.T) {
	t.Parallel()
	modules := checksandalerts.ModuleDirs{OfficialExtra: officialExtraModules(t), External: filepath.Join(t.TempDir(), "external_modules")}
	if module, e := config.DescribeModule(modules, "discord"); e != nil || module.Kind != "official_extra" || module.ShippedWithMMWatchdogVersion != checksandalerts.Version || module.Description.UsedAs != "alert_destination" || len(module.Description.Examples) == 0 {
		t.Fatalf("%+v %v", module, e)
	}
	directory := t.TempDir()
	if e := os.Mkdir(filepath.Join(directory, "check_groups"), 0700); e != nil {
		t.Fatal(e)
	}
	for file, content := range map[string]string{
		"mmwatchdog_node_settings.yml":              "keep_undelivered_alerts_for: 24h\nundelivered_alerts_content_size_limit: 8MiB\nalert_severity_when_this_node_goes_silent: sos\nalert_severity_when_this_node_has_rebooted: info\nkeep_measurement_history_for: 7d\nmeasurement_history_size_limit: 256MiB\n",
		filepath.Join("check_groups", "checks.yml"): "check_groups: {app: {check_every: 30s, time_limit_per_run: 5s, alert_after_failing_for: 1m, recovered_after_healthy_for: 30s, systemd_unit_state: {systemd_unit: myapp.service, fail_when: the_unit_is_not_active}, alert_severity: warn}}\n",
	} {
		if e := os.WriteFile(filepath.Join(directory, file), []byte(content), 0600); e != nil {
			t.Fatal(e)
		}
	}
	for _, tc := range []struct{ destination, want string }{
		{"discord: {webhook_url_file: /etc/mmwatchdog/secrets/discord-webhook-team}", ""},
		{"discord: {webhook_url_file: /etc/mmwatchdog/secrets/discord-webhook-team, mention: '<@123456789012345678>'}", ""},
		{"discord: {webhook_url_file: /etc/mmwatchdog/secrets/discord-webhook-team, mention: '@everyone', run_as_user_account: alerts}", ""},
		{"discord: {webhook_url_file: /etc/mmwatchdog/secrets/discord, mention: '<@!123456789012345678>'}", "destinations.chat.discord.mention: "},
		{"discord: {webhook_url_file: /etc/mmwatchdog/secrets/discord, mention: '@Barbara'}", "destinations.chat.discord.mention: "},
		{"discord: {webhook_url_file: /etc/mmwatchdog/secrets/discord, mention: '<@>'}", "destinations.chat.discord.mention: "},
		{"discord: {webhook_url_file: /etc/mmwatchdog/secrets/discord, mention: '<#103735883630395392>'}", "destinations.chat.discord.mention: "},
		{"discord: {webhook_url_file: /etc/mmwatchdog/secrets/discord, mention: '<@111> <@222>'}", "destinations.chat.discord.mention: "},
		{"discord: {webhook_url_file: /etc/mmwatchdog/secrets/discord, mention: ''}", "destinations.chat.discord.mention: "},
		{"discord: {mention: '@here'}", "destinations.chat.discord.webhook_url_file: is required: The absolute path of a file that holds the address of the Discord webhook, and nothing else."},
		{"discord: {webhook_url_file: secrets/discord}", "destinations.chat.discord.webhook_url_file: an input whose name ends in _file names a file that MMWatchdog reads and gives to the module. Set webhook_url_file to an absolute file path"},
		{"discord: {webhook_url_file: /etc/mmwatchdog/secrets/discord, mention: 123456789012345678}", "destinations.chat.discord.mention: expected a string"},
		{"discord: {webhook_url_file: /etc/mmwatchdog/secrets/discord, username: MMWatchdog}", `destinations.chat.discord.username: unknown field "username": the inputs of the module discord are webhook_url_file, mention`},
		{"discord: {webhook_url_file: /etc/mmwatchdog/secrets/discord, thread_id: '1300000000000000003'}", `destinations.chat.discord.thread_id: unknown field "thread_id": the inputs of the module discord are webhook_url_file, mention`},
	} {
		if e := os.WriteFile(filepath.Join(directory, "alert_destinations.yml"), []byte("destinations:\n  chat:\n    "+tc.destination+"\n    remind_every: never\n    alert_severities: [sos]\n"), 0600); e != nil {
			t.Fatal(e)
		}
		_, e := config.LoadWithModuleDirs(directory, modules)
		if tc.want == "" && e != nil || tc.want != "" && (e == nil || !strings.Contains(e.Error(), tc.want)) {
			t.Errorf("%s:\n got %v\nwant %q", tc.destination, e, tc.want)
		}
	}
}
