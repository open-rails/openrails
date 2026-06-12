package subscriptions

import (
	"bytes"

	solanago "github.com/gagliardetto/solana-go"
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

// UpdatePlanParams are the inputs for update_plan (discriminator 8): the plan's
// MUTABLE fields — status, endTs, pullers, metadataUri (the IDL updatePlanData
// struct). The core terms (amount / period / mint / destinations) are immutable.
// The program overwrites ALL four mutable fields, so callers must echo the
// current on-chain values for anything they do not intend to change.
type UpdatePlanParams struct {
	Owner   solanago.PublicKey // signer; must be the plan's on-chain owner
	PlanPDA solanago.PublicKey // writable

	Status      uint8 // PlanStatusSunset | PlanStatusActive
	EndTs       int64
	Pullers     [4]solanago.PublicKey
	MetadataURI string // <= 128 bytes
}

// BuildUpdatePlan builds the update_plan instruction (IDL: updatePlan).
// Accounts (IDL order): owner(s), planPda(w). Data: u8 discriminator,
// updatePlanData{status u8, endTs i64, pullers [4]pubkey, metadataUri byte[128]}.
func BuildUpdatePlan(p UpdatePlanParams) (solanago.Instruction, error) {
	var buf bytes.Buffer
	buf.WriteByte(discUpdatePlan)
	buf.WriteByte(p.Status)
	buf.Write(i64le(p.EndTs))
	for _, k := range p.Pullers {
		buf.Write(k.Bytes())
	}
	uri, err := writeFixedString(p.MetadataURI, metadataURILen)
	if err != nil {
		return nil, err
	}
	buf.Write(uri)

	accounts := solanago.AccountMetaSlice{
		solanago.NewAccountMeta(p.Owner, false, true),
		solanago.NewAccountMeta(p.PlanPDA, true, false),
	}
	return solanago.NewInstruction(ProgramID, accounts, buf.Bytes()), nil
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

// discATACreateIdempotent is the SPL Associated-Token-Account program's
// CreateIdempotent instruction selector (a no-op if the ATA already exists).
const discATACreateIdempotent byte = 1

// CreateIdempotentATAParams are inputs for the associated-token CreateIdempotent.
type CreateIdempotentATAParams struct {
	Payer        solanago.PublicKey // signer + writable; funds rent if the ATA is created
	ATA          solanago.PublicKey // writable; derive via DeriveATA(Owner, Mint, TokenProgram)
	Owner        solanago.PublicKey // the wallet the ATA belongs to
	Mint         solanago.PublicKey
	TokenProgram solanago.PublicKey
}

// BuildCreateIdempotentATA builds the SPL Associated-Token-Account program's
// CreateIdempotent instruction for (Owner, Mint). It creates the receiving ATA
// if absent and is a no-op if it already exists, so it is safe to repeat.
// Accounts (ATA-program order): payer(s,w), ata(w), owner, mint, systemProgram,
// tokenProgram. Data: [1] (CreateIdempotent selector).
func BuildCreateIdempotentATA(p CreateIdempotentATAParams) solanago.Instruction {
	accounts := solanago.AccountMetaSlice{
		solanago.NewAccountMeta(p.Payer, true, true),
		solanago.NewAccountMeta(p.ATA, true, false),
		solanago.NewAccountMeta(p.Owner, false, false),
		solanago.NewAccountMeta(p.Mint, false, false),
		solanago.NewAccountMeta(solanago.SystemProgramID, false, false),
		solanago.NewAccountMeta(p.TokenProgram, false, false),
	}
	return solanago.NewInstruction(AssociatedTokenProgramID, accounts, []byte{discATACreateIdempotent})
}

// BuildRevokeDelegation builds revoke_delegation (3) — closes a delegation /
// subscription account after expiry, reclaiming rent. The program's subscription
// branch REQUIRES a trailing plan_pda account; omitting it is rejected on-chain
// with Custom:113 (NotEnoughAccountKeys). Accounts (program order):
// authority(s,w), delegationAccount(w), planPda. An optional trailing receiver
// is accepted by the program but is not required to close, so we omit it.
func BuildRevokeDelegation(authority, delegationAccount, planPDA solanago.PublicKey) solanago.Instruction {
	accounts := solanago.AccountMetaSlice{
		solanago.NewAccountMeta(authority, true, true),
		solanago.NewAccountMeta(delegationAccount, true, false),
		solanago.NewAccountMeta(planPDA, false, false),
	}
	return solanago.NewInstruction(ProgramID, accounts, []byte{discRevokeDelegation})
}
