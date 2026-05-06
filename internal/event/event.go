package event

import (
	"time"
)

type Kind string

const (
	KindDeploy             Kind = "deploy"
	KindMachineStarted     Kind = "machine_started"
	KindMachineStopped     Kind = "machine_stopped"
	KindMachineExit        Kind = "machine_exit"
	KindMachineOOM         Kind = "machine_oom"
	KindMachineCrashed     Kind = "machine_crashed"
	KindMachineCreated     Kind = "machine_created"
	KindMachineDestroyed   Kind = "machine_destroyed"
	KindMachineEvent       Kind = "machine_event"
	KindHealthCheckFailing Kind = "healthcheck_failing"
	KindHealthCheckPassing Kind = "healthcheck_passing"
	KindCrashLoop          Kind = "crash_loop"
	KindCapacityDegraded   Kind = "capacity_degraded"
	KindCapacityRestored   Kind = "capacity_restored"
	KindDigest             Kind = "digest"
)

type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

type Event struct {
	Kind      Kind
	Severity  Severity
	App       string
	Region    string
	MachineID string
	Timestamp time.Time
	Title     string
	Detail    string
	Fields    map[string]string
	Payload   any
}
