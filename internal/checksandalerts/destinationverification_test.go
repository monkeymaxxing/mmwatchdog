package checksandalerts

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type verificationExample struct {
	Webhook
	accepted bool
}

func (v *verificationExample) VerifyDestination(ctx context.Context, delivery DeliveryOf) (bool, string) {
	if _, limited := ctx.Deadline(); !limited {
		return false, "no time limit"
	}
	token, err := delivery.Credential("bearer_token_file")
	if err != nil || token != "a-long-verification-token" {
		return false, "wrong credential"
	}
	return v.accepted, "verified address and credential"
}

func TestCoreDestinationVerificationReadsCredentialsWithoutDelivery(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("a-long-verification-token"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, accepted := range []bool{false, true} {
		delivery := DeliveryOf{Destination: Destination{RemindEvery: "never", Inputs: &verificationExample{Webhook: Webhook{BearerTokenFile: path}, accepted: accepted}, TimeLimitPerDeliveryAttempt: Duration(time.Minute.String())}}
		result := delivery.VerifyDestination(context.Background())
		if !result.CanVerifyWithoutMessage || result.AcceptsAddressAndCredential != accepted || result.VerificationMessage != "verified address and credential" {
			t.Fatal(result)
		}
		delivery.FileContents = map[string]string{"bearer_token_file": "a-long-verification-token"}
		delivery.Destination.Inputs.(*verificationExample).BearerTokenFile = "/not-on-this-node"
		if result = delivery.VerifyDestination(context.Background()); result.AcceptsAddressAndCredential != accepted || result.VerificationMessage != "verified address and credential" {
			t.Fatal(result)
		}
	}
	unsupported := (DeliveryOf{Destination: Destination{RemindEvery: "never", Inputs: &MMHotlineDestination{}}}).VerifyDestination(context.Background())
	if unsupported.CanVerifyWithoutMessage || unsupported.VerificationMessage != TestedOnlyByAMessage {
		t.Fatal(unsupported)
	}
}
