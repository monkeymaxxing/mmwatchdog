package checksandalerts

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"mmwatchdog/internal/primitives"
)

// VerifiesDestination is optional for a core destination's inputs. The
// implementation checks the address and credential without sending a message.
// Delivery carries credentials read by MMWatchdog, as for an alert delivery.
type VerifiesDestination interface {
	VerifyDestination(context.Context, DeliveryOf) (accepted bool, message string)
}

// DestinationVerification distinguishes a refusal from an unsupported module.
type DestinationVerification struct {
	CanVerifyWithoutMessage     bool   `json:"can_verify_without_message"`
	AcceptsAddressAndCredential bool   `json:"accepts_address_and_credential"`
	VerificationMessage         string `json:"verification_message"`
}

// TestedOnlyByAMessage is the verification message of a destination whose
// module cannot check its address and credential without sending a
// message: neither a core module with VerifyDestination nor a Starlark
// script with verify_destination.
const TestedOnlyByAMessage = "MMWatchdog can test the destination only by sending a message"

// VerifyDestination checks one destination without a message, under its
// delivery time limit. Reload and the daily schedule call this operation.
func (delivery DeliveryOf) VerifyDestination(ctx context.Context) DestinationVerification {
	destination := delivery.Destination
	core, hasCoreHook := destination.Inputs.(VerifiesDestination)
	module, isModule := destination.Inputs.(*ModuleCall)
	if !hasCoreHook && (!isModule || module.Module == nil || !module.Module.IsStarlark()) {
		return DestinationVerification{VerificationMessage: TestedOnlyByAMessage}
	}
	ctx, cancel := context.WithTimeout(ctx, destination.TimeLimitPerDeliveryAttempt.Value())
	defer cancel()
	failure := func(err error) DestinationVerification {
		return DestinationVerification{CanVerifyWithoutMessage: true, VerificationMessage: primitives.DetailLine(WithoutFileContents(err.Error(), delivery.FileContents))}
	}
	if !hasCoreHook {
		path := filepath.Join(module.Module.Directory, ModuleScriptFile)
		source, err := primitives.ReadFile(path, moduleScriptLimit)
		if err != nil {
			return failure(err)
		}
		hasHook, err := scriptDeclaresName(path, source, "verify_destination")
		if err != nil {
			return failure(err)
		}
		if !hasHook {
			return DestinationVerification{VerificationMessage: TestedOnlyByAMessage}
		}
	}
	if delivery.FileContents == nil {
		contents, input, err := destination.ReadCredentialFiles()
		if err != nil {
			return failure(fmt.Errorf("%s: %w", input, err))
		}
		delivery.FileContents = contents
	}
	if hasCoreHook {
		accepted, message := core.VerifyDestination(ctx, delivery)
		if err := ctx.Err(); err != nil {
			return failure(err)
		}
		return DestinationVerification{true, accepted, primitives.DetailLine(WithoutFileContents(message, delivery.FileContents))}
	}
	request, err := PlainJSON(map[string]any{"protocol": 1, "verify_destination": true, "inputs": ModuleInputs(module.Inputs), "file_contents": delivery.FileContents, "mmwatchdog_ends_the_module_after_milliseconds": mmwatchdogEndsTheModuleAfter(ctx)})
	if err != nil {
		return failure(err)
	}
	ran, err := module.start(ctx, request)
	if err != nil {
		return failure(err)
	}
	if ran.ExitCode != 0 {
		return failure(fmt.Errorf("verification module exited with status %d: %s", ran.ExitCode, ran.StandardError))
	}
	var result struct {
		Protocol                    int    `json:"protocol"`
		CanVerifyWithoutMessage     bool   `json:"can_verify_without_message"`
		AcceptsAddressAndCredential bool   `json:"accepts_address_and_credential"`
		VerificationMessage         string `json:"verification_message"`
	}
	if err := json.Unmarshal([]byte(ran.StandardOutput), &result); err != nil {
		return failure(err)
	}
	if result.Protocol != 1 {
		return failure(fmt.Errorf("destination verification result must use protocol: 1"))
	}
	return DestinationVerification{result.CanVerifyWithoutMessage, result.AcceptsAddressAndCredential, primitives.DetailLine(WithoutFileContents(result.VerificationMessage, delivery.FileContents))}
}
