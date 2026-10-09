package email

import (
	"context"
	"fmt"
	"html"
	"strings"

	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/internal/config"
)

// app names the product in the control plane's messages: its users sign in to
// OpenRails itself.
const app = "OpenRails"

// AuthKitSender hands the standalone server's AuthKit messages, rendered, to
// the engine's email sender. A host with its own templates gives the server an
// authkit.EmailSender instead.
type AuthKitSender struct{ Sender config.EmailSender }

// Send renders msg and delivers it from the deployment's own address.
func (a AuthKitSender) Send(ctx context.Context, msg iam.EmailMessage) error {
	subject, text, err := render(msg)
	if err != nil {
		return err
	}
	return a.Sender.Send(ctx, config.Email{To: msg.To, Subject: subject, Text: text, HTML: htmlOf(text)})
}

// CheckHealth is the sender's.
func (a AuthKitSender) CheckHealth(ctx context.Context) error { return a.Sender.CheckHealth(ctx) }

func render(msg iam.EmailMessage) (subject, text string, err error) {
	code, link := strings.TrimSpace(msg.Code), strings.TrimSpace(msg.Link)
	switch msg.Kind {
	case iam.MessageVerification:
		intro := "Use the following verification details:"
		if msg.Purpose == iam.PurposeContactChange {
			intro = "Use the following details to confirm this change:"
		}
		lines := []string{intro}
		if code != "" {
			lines = append(lines, "Code: "+code)
		}
		if link != "" {
			lines = append(lines, "Verify link: "+link)
		}
		return fmt.Sprintf("Verify your %s account", app), strings.Join(lines, "\n"), nil
	case iam.MessageLoginCode:
		return fmt.Sprintf("Your %s login code", app), "Login code: " + code, nil
	case iam.MessagePasswordReset:
		return fmt.Sprintf("Reset your %s password", app), "Use this link to reset your password:\n" + link, nil
	case iam.MessageInvite:
		return fmt.Sprintf("You're invited to %s", app), fmt.Sprintf("You've been invited to join %s. Follow the link to create your account:\n%s", app, link), nil
	case iam.MessageWelcome:
		return fmt.Sprintf("Welcome to %s", app), fmt.Sprintf("Welcome to %s.", app), nil
	case iam.MessageContactChanged:
		ch := msg.ContactChange
		if ch == nil {
			return "", "", fmt.Errorf("email: contact_changed needs a ContactChange")
		}
		return fmt.Sprintf("Your %s %s was changed", app, ch.Field),
			fmt.Sprintf("The %s on your %s account was changed to %s. If this was not you, secure your account now.", ch.Field, app, ch.NewValue), nil
	case iam.MessageDeviceKeyEnrolled:
		key := msg.DeviceKey
		if key == nil {
			return "", "", fmt.Errorf("email: device_key_enrolled needs a DeviceKey")
		}
		device := strings.TrimSpace(key.Label)
		if device == "" {
			device = "A new device"
		}
		return fmt.Sprintf("A new device can sign in to your %s account", app),
			fmt.Sprintf("%s was enrolled on your %s account on %s and can now sign in as you. If this was not you, revoke it and secure your email now.", device, app, key.CreatedAt.UTC().Format("2006-01-02 15:04 UTC")), nil
	case iam.MessageMFAReset:
		return fmt.Sprintf("Two-step verification on your %s account was reset", app),
			fmt.Sprintf("An administrator removed the passkeys, second factors, backup codes and device keys of your %s account and signed it out everywhere. Set up two-step verification again when you next sign in. If you did not ask for this, contact support now.", app), nil
	}
	return "", "", fmt.Errorf("email: no template for AuthKit message kind %q", msg.Kind)
}

func htmlOf(text string) string {
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		b.WriteString("<p>" + html.EscapeString(line) + "</p>")
	}
	return b.String()
}
