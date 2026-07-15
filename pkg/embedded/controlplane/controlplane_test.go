package controlplane

import (
	"context"
	"testing"

	authcore "github.com/open-rails/authkit/embedded"
)

type passwordlessTestSender struct{}

type passwordlessTestSMSSender struct{}

func (passwordlessTestSender) SendVerification(context.Context, string, string, authcore.VerificationMessage) error {
	return nil
}

func (passwordlessTestSender) SendPasswordResetLink(context.Context, string, string, string) error {
	return nil
}

func (passwordlessTestSender) SendAccountRegistrationInvite(context.Context, string, string) error {
	return nil
}

func (passwordlessTestSender) SendLoginCode(context.Context, string, string, string) error {
	return nil
}

func (passwordlessTestSender) SendWelcome(context.Context, string, string) error {
	return nil
}

func (passwordlessTestSMSSender) SendVerification(context.Context, string, authcore.VerificationMessage) error {
	return nil
}

func (passwordlessTestSMSSender) SendPasswordResetLink(context.Context, string, string) error {
	return nil
}

func (passwordlessTestSMSSender) SendLoginCode(context.Context, string, string) error {
	return nil
}

func TestValidateAttachOptionsPasswordlessPolicy(t *testing.T) {
	var typedNilSender *passwordlessTestSender
	tests := []struct {
		name    string
		opts    AttachOptions
		wantErr bool
	}{
		{name: "disabled", opts: AttachOptions{}},
		{
			name: "login only",
			opts: AttachOptions{PasswordlessLogin: true, EmailSender: passwordlessTestSender{}},
		},
		{
			name: "hosted auto-registration",
			opts: AttachOptions{
				HostedPosture:                true,
				PasswordlessLogin:            true,
				PasswordlessAutoRegistration: true,
				EmailSender:                  passwordlessTestSender{},
			},
		},
		{
			name:    "auto-registration without login",
			opts:    AttachOptions{PasswordlessAutoRegistration: true, EmailSender: passwordlessTestSender{}},
			wantErr: true,
		},
		{
			name: "auto-registration without hosted posture",
			opts: AttachOptions{
				PasswordlessLogin:            true,
				PasswordlessAutoRegistration: true,
				EmailSender:                  passwordlessTestSender{},
			},
			wantErr: true,
		},
		{
			name:    "login without sender",
			opts:    AttachOptions{PasswordlessLogin: true},
			wantErr: true,
		},
		{
			name: "login with typed nil sender",
			opts: AttachOptions{
				PasswordlessLogin: true,
				EmailSender:       typedNilSender,
			},
			wantErr: true,
		},
		{
			name: "login with typed nil email and valid SMS sender",
			opts: AttachOptions{
				PasswordlessLogin: true,
				EmailSender:       typedNilSender,
				SMSSender:         passwordlessTestSMSSender{},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateAttachOptions(test.opts)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateAttachOptions() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}
