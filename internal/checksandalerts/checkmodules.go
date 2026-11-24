package checksandalerts

import "mmwatchdog/internal/primitives"

// The topics that group the core modules, in the order of the index of the
// module pages (docs/reference/modules/README.md) and of "mmwatchdog describe-module". Each
// line of the two registries names one of them; a test checks that every
// topic has a module.
const (
	topicServicesAndProtocols      = "Services and protocols"
	topicDatabasesCachesContainers = "Databases, caches, and containers"
	topicSystemdUnits              = "Systemd units"
	topicNodeResourcesStorage      = "The node's resources, storage, and hardware"
	topicNodeSoftwareClockKernel   = "The node's software, clock, and kernel"
	topicFilesLogsCommands         = "Files, logs, and commands"
	topicAlertDestinations         = "Alert destinations"
)

// CoreModuleTopics are the topics of the core modules, in their order.
var CoreModuleTopics = []string{
	topicServicesAndProtocols,
	topicDatabasesCachesContainers,
	topicSystemdUnits,
	topicNodeResourcesStorage,
	topicNodeSoftwareClockKernel,
	topicFilesLogsCommands,
	topicAlertDestinations,
}

// CoreCheckModules is the registry of the core check modules: one line for
// each, grouped by topic. A module's input type, the checks of its inputs,
// its code, and its description are in its own file, check_NAME.go, and its
// page source is check_NAME.md beside it. module.go says what each field
// means.
var CoreCheckModules = []CheckModule{
	{Name: "http", Topic: topicServicesAndProtocols, Description: &HTTPDescription, NewInputs: func() CheckInputs { return &HTTP{} }},
	{Name: "tcp", Topic: topicServicesAndProtocols, Description: &tcpDescription, NewInputs: func() CheckInputs { return &TCP{} }, ScalarStandsFor: "an address as host:port"},
	{Name: "udp", Topic: topicServicesAndProtocols, Description: &udpDescription, NewInputs: func() CheckInputs { return &UDP{} }},
	{Name: "unix_socket", Topic: topicServicesAndProtocols, Description: &unixSocketDescription, NewInputs: func() CheckInputs { return new(UnixSocket) }},
	{Name: "ping", Topic: topicServicesAndProtocols, Description: &pingDescription, NewInputs: func() CheckInputs { return new(Ping) }, LimitsOnItsMeasurements: true},
	{Name: "tls", Topic: topicServicesAndProtocols, Description: &tlsDescription, NewInputs: func() CheckInputs { return &TLS{} }},
	{Name: "ssh", Topic: topicServicesAndProtocols, Description: &sshDescription, NewInputs: func() CheckInputs { return new(SSH) }},
	{Name: "smtp", Topic: topicServicesAndProtocols, Description: &smtpDescription, NewInputs: func() CheckInputs { return &SMTP{} }},
	{Name: "imap", Topic: topicServicesAndProtocols, Description: &imapDescription, NewInputs: func() CheckInputs { return &IMAP{} }},
	{Name: "pop3", Topic: topicServicesAndProtocols, Description: &pop3Description, NewInputs: func() CheckInputs { return &POP3{} }},
	{Name: "dns", Topic: topicServicesAndProtocols, Description: &dnsDescription, NewInputs: func() CheckInputs { return &DNS{} }},
	{Name: "memcached", Topic: topicDatabasesCachesContainers, Description: &memcachedDescription, NewInputs: func() CheckInputs { return new(Memcached) }, LimitsOnItsMeasurements: true},
	{Name: "redis", Topic: topicDatabasesCachesContainers, Description: &redisDescription, NewInputs: func() CheckInputs { return &Redis{} }},
	{Name: "postgresql", Topic: topicDatabasesCachesContainers, Description: &postgresqlDescription, NewInputs: func() CheckInputs { return &PostgreSQL{} }, LimitsOnItsMeasurements: true},
	{Name: "pgbackrest", Topic: topicDatabasesCachesContainers, Description: &pgbackrestDescription, NewInputs: func() CheckInputs { return &PgBackRest{} }, LimitsOnItsMeasurements: true},
	{Name: "mysql", Topic: topicDatabasesCachesContainers, Description: &mysqlDescription, NewInputs: func() CheckInputs { return &MySQL{} }, LimitsOnItsMeasurements: true},
	{Name: "sqlite_database", Topic: topicDatabasesCachesContainers, Description: &sqliteDatabaseDescription, NewInputs: func() CheckInputs { return &SQLiteDatabase{} }, LimitsOnItsMeasurements: true},
	{Name: "docker_container", Topic: topicDatabasesCachesContainers, Description: &dockerContainerDescription, NewInputs: func() CheckInputs { return new(DockerContainer) }},
	{Name: "systemd_unit_state", Topic: topicSystemdUnits, Description: &systemdUnitStateDescription, NewInputs: func() CheckInputs { return &SystemdUnitState{} }},
	{Name: "systemd_unit_commands", Topic: topicSystemdUnits, Description: &systemdUnitCommandsDescription, NewInputs: func() CheckInputs { return &SystemdUnitCommands{} }},
	{Name: "systemd_unit_crash_loop", Topic: topicSystemdUnits, Description: &systemdUnitCrashLoopDescription, NewInputs: func() CheckInputs { return &SystemdUnitCrashLoop{} }},
	{Name: "every_systemd_unit", Topic: topicSystemdUnits, Description: &everySystemdUnitDescription, NewInputs: func() CheckInputs { return &EverySystemdUnit{} }, OneIncidentForEachFinding: true, findingIsGoneMessage: primitives.UnitIsNoLongerFailed},
	{Name: "systemd_unit_resource", Topic: topicSystemdUnits, Description: &systemdUnitResourceDescription, NewInputs: func() CheckInputs { return &SystemdUnitResource{} }, Measures: true},
	{Name: "node_resource", Topic: topicNodeResourcesStorage, Description: &nodeResourceDescription, NewInputs: func() CheckInputs { return new(NodeResource) }, Measures: true},
	{Name: "node_filesystem", Topic: topicNodeResourcesStorage, Description: &nodeFilesystemDescription, NewInputs: func() CheckInputs { return new(NodeFilesystem) }, Measures: true, FailsWithoutALimit: true},
	{Name: "every_disk_and_network_filesystem", Topic: topicNodeResourcesStorage, Description: &everyMountedFilesystemDescription, NewInputs: func() CheckInputs { return &EveryDiskAndNetworkFilesystem{} }, LimitsOnItsMeasurements: true, OneIncidentForEachFinding: true, findingIsGoneMessage: filesystemIsOkAgain},
	{Name: "node_disk_device", Topic: topicNodeResourcesStorage, Description: &nodeDiskDeviceDescription, NewInputs: func() CheckInputs { return new(NodeDiskDevice) }, Measures: true},
	{Name: "node_network_interface", Topic: topicNodeResourcesStorage, Description: &nodeNetworkInterfaceDescription, NewInputs: func() CheckInputs { return new(NodeNetworkInterface) }, Measures: true, FailsWithoutALimit: true},
	{Name: "node_temperature", Topic: topicNodeResourcesStorage, Description: &nodeTemperatureDescription, NewInputs: func() CheckInputs { return new(NodeTemperature) }, Measures: true},
	{Name: "mdadm_array", Topic: topicNodeResourcesStorage, Description: &mdadmArrayDescription, NewInputs: func() CheckInputs { return new(MdadmArray) }},
	{Name: "apt_security_updates", Topic: topicNodeSoftwareClockKernel, Description: &aptSecurityUpdatesDescription, NewInputs: func() CheckInputs { return new(AptSecurityUpdates) }, OneIncidentForEachFinding: true, findingIsGoneMessage: noSecurityUpdateWaits},
	{Name: "dnf_security_updates", Topic: topicNodeSoftwareClockKernel, Description: &dnfSecurityUpdatesDescription, NewInputs: func() CheckInputs { return new(DnfSecurityUpdates) }, OneIncidentForEachFinding: true, findingIsGoneMessage: noSecurityUpdateWaits},
	{Name: "zypper_security_patches", Topic: topicNodeSoftwareClockKernel, Description: &zypperSecurityPatchesDescription, NewInputs: func() CheckInputs { return new(ZypperSecurityPatches) }, OneIncidentForEachFinding: true, findingIsGoneMessage: noSecurityPatchWaits},
	{Name: "ubuntu_pro_security_fixes", Topic: topicNodeSoftwareClockKernel, Description: &ubuntuProSecurityFixesDescription, NewInputs: func() CheckInputs { return new(UbuntuProSecurityFixes) }, OneIncidentForEachFinding: true, findingIsGoneMessage: ubuntuProSecurityFixIsGone},
	{Name: "reboot_after_updates", Topic: topicNodeSoftwareClockKernel, Description: &rebootAfterUpdatesDescription, NewInputs: func() CheckInputs { return new(RebootAfterUpdates) }},
	{Name: "distribution_release_security_support", Topic: topicNodeSoftwareClockKernel, Description: &distributionReleaseSecuritySupportDescription, NewInputs: func() CheckInputs { return new(DistributionReleaseSecuritySupport) }},
	{Name: "known_exploited_vulnerabilities_in_installed_packages", Topic: topicNodeSoftwareClockKernel, Description: &knownExploitedVulnerabilitiesInInstalledPackagesDescription, NewInputs: func() CheckInputs { return new(KnownExploitedVulnerabilitiesInInstalledPackages) }, OneIncidentForEachFinding: true, findingIsGoneMessage: knownExploitedVulnerabilityIsGone},
	{Name: "node_clock", Topic: topicNodeSoftwareClockKernel, Description: &nodeClockDescription, NewInputs: func() CheckInputs { return new(NodeClock) }},
	{Name: "out_of_memory_kills", Topic: topicNodeSoftwareClockKernel, Description: &outOfMemoryKillsDescription, NewInputs: func() CheckInputs { return new(OutOfMemoryKills) }, OpensIncidentAtFirstEvent: func(CheckInputs) bool { return true }},
	{Name: "file", Topic: topicFilesLogsCommands, Description: &fileDescription, NewInputs: func() CheckInputs { return &File{} }, LimitsOnItsMeasurements: true},
	{Name: "log_lines", Topic: topicFilesLogsCommands, Description: &logLinesDescription, NewInputs: func() CheckInputs { return &LogLines{} }, OpensIncidentAtFirstEvent: func(inputs CheckInputs) bool {
		log := inputs.(*LogLines)
		return log.FailWhenNoNewLineFor == ""
	}},
	{Name: "shell", Topic: topicFilesLogsCommands, Description: &shellDescription, NewInputs: func() CheckInputs { return &Shell{} }, ScalarStandsFor: "a Bash string"},
}
