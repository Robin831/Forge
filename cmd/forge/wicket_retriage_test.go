package main

import (
	"testing"
)

func TestWicketRetrageCmd_ArgsValidation(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{
			name:    "missing args",
			args:    []string{},
			wantErr: true,
		},
		{
			name:    "missing issue number",
			args:    []string{"owner/repo"},
			wantErr: true,
		},
		{
			name:    "too many args",
			args:    []string{"owner/repo", "42", "extra"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := wicketRetrageCmd.Args(wicketRetrageCmd, tt.args)
			if tt.wantErr && err == nil {
				t.Error("expected error for invalid args, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestWicketRetrageCmd_ValidArgs(t *testing.T) {
	err := wicketRetrageCmd.Args(wicketRetrageCmd, []string{"owner/repo", "42"})
	if err != nil {
		t.Errorf("expected no error for valid args, got: %v", err)
	}
}
