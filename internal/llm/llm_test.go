package llm

import (
	"strings"
	"testing"
)

func TestSelect_ExplicitKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv(BackendEnvVar, "")
	c, label, err := Select("sk-ant-explicit")
	if err != nil {
		t.Fatal(err)
	}
	if label != BackendAnthropicSDK {
		t.Errorf("label = %q, want %q", label, BackendAnthropicSDK)
	}
	if c == nil {
		t.Fatal("client is nil")
	}
}

func TestSelect_EnvKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-env")
	t.Setenv(BackendEnvVar, "")
	_, label, err := Select("")
	if err != nil {
		t.Fatal(err)
	}
	if label != BackendAnthropicSDK {
		t.Errorf("label = %q, want %q", label, BackendAnthropicSDK)
	}
}

func TestSelect_ForcedUnknown(t *testing.T) {
	t.Setenv(BackendEnvVar, "magic-llm")
	_, _, err := Select("sk-ant")
	if err == nil {
		t.Fatal("expected error for unknown backend")
	}
	if !strings.Contains(err.Error(), "magic-llm") {
		t.Errorf("error should name unknown backend: %v", err)
	}
}

func TestSelect_ForcedSDK(t *testing.T) {
	t.Setenv(BackendEnvVar, BackendAnthropicSDK)
	_, label, err := Select("sk-ant-forced")
	if err != nil {
		t.Fatal(err)
	}
	if label != BackendAnthropicSDK {
		t.Errorf("label = %q", label)
	}
}
