package subscriptions

import (
	"bytes"

	solanago "github.com/doujins-org/solana-go"
)

// CreatePlanParams are the inputs for create_plan (discriminator 7).
type CreatePlanParams struct {
	Merchant     solanago.PublicKey // signer + writable; plan owner
	PlanPDA      solanago.PublicKey // derive via DerivePlanPDA(Merchant, PlanID)
	Mint         solanago.PublicKey
	TokenProgram solanago.PublicKey

	PlanID       uint64
	Terms        PlanTerms
	EndTs        int64                 // 0 = perpetual
	Destinations [4]solanago.PublicKey // whitelisted receivers (zero pubkey = unused slot)
	Pullers      [4]solanago.PublicKey // whitelisted callers (zero pubkey = unused slot)
	MetadataURI  string                // <= 128 bytes
}

// BuildCreatePlan builds the create_plan instruction. Accounts (IDL order):
// merchant(s,w), planPda(w), tokenMint, systemProgram, tokenProgram.
func BuildCreatePlan(p CreatePlanParams) (solanago.Instruction, error) {
	data, err := encodeCreatePlan(p)
	if err != nil {
		return nil, err
	}
	accounts := solanago.AccountMetaSlice{
		solanago.NewAccountMeta(p.Merchant, true, true),
		solanago.NewAccountMeta(p.PlanPDA, true, false),
		solanago.NewAccountMeta(p.Mint, false, false),
		solanago.NewAccountMeta(solanago.SystemProgramID, false, false),
		solanago.NewAccountMeta(p.TokenProgram, false, false),
	}
	return solanago.NewInstruction(ProgramID, accounts, data), nil
}

func encodeCreatePlan(p CreatePlanParams) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte(discCreatePlan)
	buf.Write(u64le(p.PlanID))
	buf.Write(p.Mint.Bytes())
	// terms
	buf.Write(u64le(p.Terms.Amount))
	buf.Write(u64le(p.Terms.PeriodHours))
	buf.Write(i64le(p.Terms.CreatedAt))
	// endTs
	buf.Write(i64le(p.EndTs))
	// destinations[4], pullers[4]
	for _, d := range p.Destinations {
		buf.Write(d.Bytes())
	}
	for _, d := range p.Pullers {
		buf.Write(d.Bytes())
	}
	uri, err := writeFixedString(p.MetadataURI, 128)
	if err != nil {
		return nil, err
	}
	buf.Write(uri)
	return buf.Bytes(), nil
}

// InitSubscriptionAuthorityParams are inputs for initialize_subscription_authority (0).
type InitSubscriptionAuthorityParams struct {
	Owner                 solanago.PublicKey // signer + writable
	SubscriptionAuthority solanago.PublicKey // derive via DeriveSubscriptionAuthority
	TokenMint             solanago.PublicKey
	UserATA               solanago.PublicKey
	TokenProgram          solanago.PublicKey
}

// BuildInitSubscriptionAuthority builds initialize_subscription_authority.
// Accounts: owner(s,w), subscriptionAuthority(w), tokenMint, userAta(w),
// systemProgram, tokenProgram.
func BuildInitSubscriptionAuthority(p InitSubscriptionAuthorityParams) solanago.Instruction {
	accounts := solanago.AccountMetaSlice{
		solanago.NewAccountMeta(p.Owner, true, true),
		solanago.NewAccountMeta(p.SubscriptionAuthority, true, false),
		solanago.NewAccountMeta(p.TokenMint, false, false),
		solanago.NewAccountMeta(p.UserATA, true, false),
		solanago.NewAccountMeta(solanago.SystemProgramID, false, false),
		solanago.NewAccountMeta(p.TokenProgram, false, false),
	}
	return solanago.NewInstruction(ProgramID, accounts, []byte{discInitSubscriptionAuthority})
}

// SubscribeParams are inputs for subscribe (11).
type SubscribeParams struct {
	Subscriber               solanago.PublicKey // signer + writable
	Merchant                 solanago.PublicKey
	PlanPDA                  solanago.PublicKey
	SubscriptionPDA          solanago.PublicKey // writable
	SubscriptionAuthorityPDA solanago.PublicKey
	EventAuthority           solanago.PublicKey

	PlanID                         uint64
	PlanBump                       uint8
	ExpectedMint                   solanago.PublicKey
	ExpectedAmount                 uint64
	ExpectedPeriodHours            uint64
	ExpectedCreatedAt              int64
	ExpectedSubscriptionAuthInitID int64
}

// BuildSubscribe builds the subscribe instruction. Accounts: subscriber(s,w),
// merchant, planPda, subscriptionPda(w), subscriptionAuthorityPda, systemProgram,
// eventAuthority, selfProgram.
func BuildSubscribe(p SubscribeParams) solanago.Instruction {
	var buf bytes.Buffer
	buf.WriteByte(discSubscribe)
	buf.Write(u64le(p.PlanID))
	buf.WriteByte(p.PlanBump)
	buf.Write(p.ExpectedMint.Bytes())
	buf.Write(u64le(p.ExpectedAmount))
	buf.Write(u64le(p.ExpectedPeriodHours))
	buf.Write(i64le(p.ExpectedCreatedAt))
	buf.Write(i64le(p.ExpectedSubscriptionAuthInitID))

	accounts := solanago.AccountMetaSlice{
		solanago.NewAccountMeta(p.Subscriber, true, true),
		solanago.NewAccountMeta(p.Merchant, false, false),
		solanago.NewAccountMeta(p.PlanPDA, false, false),
		solanago.NewAccountMeta(p.SubscriptionPDA, true, false),
		solanago.NewAccountMeta(p.SubscriptionAuthorityPDA, false, false),
		solanago.NewAccountMeta(solanago.SystemProgramID, false, false),
		solanago.NewAccountMeta(p.EventAuthority, false, false),
		solanago.NewAccountMeta(ProgramID, false, false),
	}
	return solanago.NewInstruction(ProgramID, accounts, buf.Bytes())
}

// TransferSubscriptionParams are inputs for transfer_subscription (10) — the pull.
type TransferSubscriptionParams struct {
	SubscriptionPDA       solanago.PublicKey // writable
	PlanPDA               solanago.PublicKey
	SubscriptionAuthority solanago.PublicKey
	DelegatorATA          solanago.PublicKey // writable; subscriber's token account
	ReceiverATA           solanago.PublicKey // writable; merchant/destination token account
	Caller                solanago.PublicKey // signer; plan owner or whitelisted puller
	Mint                  solanago.PublicKey
	TokenProgram          solanago.PublicKey
	EventAuthority        solanago.PublicKey

	Amount    uint64
	Delegator solanago.PublicKey // the subscriber (transferData.delegator)
}

// BuildTransferSubscription builds transfer_subscription. Accounts:
// subscriptionPda(w), planPda, subscriptionAuthority, delegatorAta(w),
// receiverAta(w), caller(s), tokenMint, tokenProgram, eventAuthority, selfProgram.
func BuildTransferSubscription(p TransferSubscriptionParams) solanago.Instruction {
	var buf bytes.Buffer
	buf.WriteByte(discTransferSubscription)
	buf.Write(u64le(p.Amount))
	buf.Write(p.Delegator.Bytes())
	buf.Write(p.Mint.Bytes())

	accounts := solanago.AccountMetaSlice{
		solanago.NewAccountMeta(p.SubscriptionPDA, true, false),
		solanago.NewAccountMeta(p.PlanPDA, false, false),
		solanago.NewAccountMeta(p.SubscriptionAuthority, false, false),
		solanago.NewAccountMeta(p.DelegatorATA, true, false),
		solanago.NewAccountMeta(p.ReceiverATA, true, false),
		solanago.NewAccountMeta(p.Caller, false, true),
		solanago.NewAccountMeta(p.Mint, false, false),
		solanago.NewAccountMeta(p.TokenProgram, false, false),
		solanago.NewAccountMeta(p.EventAuthority, false, false),
		solanago.NewAccountMeta(ProgramID, false, false),
	}
	return solanago.NewInstruction(ProgramID, accounts, buf.Bytes())
}

// CancelOrResumeParams are inputs for cancel_subscription (12) / resume_subscription (13).
type CancelOrResumeParams struct {
	Subscriber      solanago.PublicKey // signer
	PlanPDA         solanago.PublicKey
	SubscriptionPDA solanago.PublicKey // writable
	EventAuthority  solanago.PublicKey
}

// BuildCancelSubscription builds cancel_subscription. Accounts: subscriber(s),
// planPda, subscriptionPda(w), eventAuthority, selfProgram.
func BuildCancelSubscription(p CancelOrResumeParams) solanago.Instruction {
	return buildCancelResume(discCancelSubscription, p)
}

// BuildResumeSubscription builds resume_subscription (same account layout).
func BuildResumeSubscription(p CancelOrResumeParams) solanago.Instruction {
	return buildCancelResume(discResumeSubscription, p)
}

func buildCancelResume(disc byte, p CancelOrResumeParams) solanago.Instruction {
	accounts := solanago.AccountMetaSlice{
		solanago.NewAccountMeta(p.Subscriber, false, true),
		solanago.NewAccountMeta(p.PlanPDA, false, false),
		solanago.NewAccountMeta(p.SubscriptionPDA, true, false),
		solanago.NewAccountMeta(p.EventAuthority, false, false),
		solanago.NewAccountMeta(ProgramID, false, false),
	}
	return solanago.NewInstruction(ProgramID, accounts, []byte{disc})
}

// BuildRevokeDelegation builds revoke_delegation (3) — closes a delegation /
// subscription account after expiry, reclaiming rent. Accounts:
// authority(s,w), delegationAccount(w).
func BuildRevokeDelegation(authority, delegationAccount solanago.PublicKey) solanago.Instruction {
	accounts := solanago.AccountMetaSlice{
		solanago.NewAccountMeta(authority, true, true),
		solanago.NewAccountMeta(delegationAccount, true, false),
	}
	return solanago.NewInstruction(ProgramID, accounts, []byte{discRevokeDelegation})
}
