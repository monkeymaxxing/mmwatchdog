package fleet

import (
	"testing"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/primitives"
)

// A joined node runs every hub destination module as the account
// mmwatchdog, from /, with no added environment, whatever the hub stated.
func TestAJoinedNodeRunsHubDestinationModulesAsMMWatchdog(t *testing.T) {
	call := &checksandalerts.ModuleCall{Name: "own_alarm", ExecOptions: primitives.ExecOptions{RunAsUserAccount: "root", RunFromDirectory: "/tmp", Env: map[string]string{"PERL5OPT": "-Mx"}}}
	s := &FleetSettings{Destinations: map[string]checksandalerts.Destination{"own_alarm": {Inputs: call}}}
	s.RunDestinationModulesAsAJoinedNode()
	if call.RunAsUserAccount != "mmwatchdog" || call.RunFromDirectory != "/" || len(call.Env) != 0 {
		t.Fatalf("%+v", call.ExecOptions)
	}
}
