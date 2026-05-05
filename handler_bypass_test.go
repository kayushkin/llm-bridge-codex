package main

import "testing"

func ptrBool(v bool) *bool { return &v }

func TestApplyStartConfig_BypassWinsOverAutoApprove(t *testing.T) {
	// auto_approve sets sandbox=workspace-write, which on Ubuntu 24.04 is
	// exactly what fails at the bwrap loopback step. Bypass must clobber it
	// to danger-full-access.
	b := &Bridge{cfg: Config{}}
	b.applyStartConfig(StartParams{
		AutoApprove:       ptrBool(true),
		BypassPermissions: true,
	})
	if b.cfg.ApprovalMode != "never" {
		t.Errorf("ApprovalMode = %q, want \"never\"", b.cfg.ApprovalMode)
	}
	if b.cfg.SandboxPolicy != "danger-full-access" {
		t.Errorf("SandboxPolicy = %q, want \"danger-full-access\"", b.cfg.SandboxPolicy)
	}
}

func TestApplyStartConfig_BypassWinsOverExplicitFields(t *testing.T) {
	b := &Bridge{cfg: Config{}}
	b.applyStartConfig(StartParams{
		ApprovalMode:      "on-request",
		Sandbox:           "workspace-write",
		BypassPermissions: true,
	})
	if b.cfg.ApprovalMode != "never" {
		t.Errorf("ApprovalMode = %q, want \"never\"", b.cfg.ApprovalMode)
	}
	if b.cfg.SandboxPolicy != "danger-full-access" {
		t.Errorf("SandboxPolicy = %q, want \"danger-full-access\"", b.cfg.SandboxPolicy)
	}
}

func TestApplyStartConfig_BypassOff_LeavesPolicyAlone(t *testing.T) {
	// With bypass off, explicit fields stay; this is the codex default path.
	b := &Bridge{cfg: Config{ApprovalMode: "on-request", SandboxPolicy: "workspace-write"}}
	b.applyStartConfig(StartParams{})
	if b.cfg.ApprovalMode != "on-request" {
		t.Errorf("ApprovalMode mutated unexpectedly: %q", b.cfg.ApprovalMode)
	}
	if b.cfg.SandboxPolicy != "workspace-write" {
		t.Errorf("SandboxPolicy mutated unexpectedly: %q", b.cfg.SandboxPolicy)
	}
}
