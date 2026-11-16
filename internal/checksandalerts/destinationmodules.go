package checksandalerts

// CoreDestinationModules is the registry of the core destination modules:
// one line for each, with its topic of CoreModuleTopics (checkmodules.go).
// A module's input type, the checks of its inputs, its sender, and its
// description are in its own file, destination_NAME.go, and its page source
// is destination_NAME.md beside it. destination.go says what each field
// means.
var CoreDestinationModules = []destinationModule{
	{Name: "webhook", Topic: topicAlertDestinations, Description: &WebhookDescription, NewInputs: func() destinationInputs { return &Webhook{} }},
	// A mail server may take seconds to accept a message, and an attempt
	// that gives up while the mail server still accepts the message sends
	// the email again.
	{Name: "email", Topic: topicAlertDestinations, Description: &emailDescription, NewInputs: func() destinationInputs { return &Email{} }, DefaultTimeLimit: "30s"},
	{Name: "system_log", Topic: topicAlertDestinations, Description: &systemLogDescription, NewInputs: func() destinationInputs { return &SystemLog{} }},
	{Name: "mmhotline", Topic: topicAlertDestinations, Description: &mmhotlineDescription, NewInputs: func() destinationInputs { return &MMHotlineDestination{} }},
}
