package billing

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// LogEntryID names one entry of a merchant's history (log_).
type LogEntryID uuid.UUID

const logEntryIDPrefix = "log_"

func ParseLogEntryID(s string) (LogEntryID, error) {
	u, err := parsePrefixedID("log entry", logEntryIDPrefix, s)
	return LogEntryID(u), err
}

func (id LogEntryID) UUID() uuid.UUID              { return uuid.UUID(id) }
func (id LogEntryID) IsZero() bool                 { return uuid.UUID(id) == uuid.Nil }
func (id LogEntryID) String() string               { return formatPrefixedID(logEntryIDPrefix, uuid.UUID(id)) }
func (id LogEntryID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id *LogEntryID) UnmarshalText(text []byte) error {
	parsed, err := ParseLogEntryID(string(text))
	*id = parsed
	return err
}

// LogActor is who made a change: staff:<invoker> (a staff member or an
// application, as the route gate admitted it), customer:<invoker>,
// provider:<rail>, or LogActorSystem, LogActorFile or LogActorVault where no
// person acted.
type LogActor string

const (
	// LogActorSystem is OpenRails' own work: a job, or operator tooling that
	// carries no identity.
	LogActorSystem LogActor = "system"
	// LogActorFile is the configuration file, edited outside OpenRails.
	LogActorFile LogActor = "file"
	// LogActorVault is a Vault document written outside OpenRails.
	LogActorVault LogActor = "vault"
)

// LogSource is how a change arrived.
type LogSource string

const (
	LogSourceAPI          LogSource = "api"
	LogSourceCustomer     LogSource = "customer"
	LogSourceConsole      LogSource = "console"
	LogSourceCatalogApply LogSource = "catalog_apply"
	LogSourceCLI          LogSource = "cli"
	// LogSourceReload is a change found when configuration loaded.
	LogSourceReload       LogSource = "reload"
	LogSourceJob          LogSource = "job"
	LogSourceIntent       LogSource = "intent"
	LogSourceWebhook      LogSource = "webhook"
	LogSourceProviderRead LogSource = "provider_read"
)

// LogObjectType is the kind of object an entry is about.
type LogObjectType string

const (
	LogObjectProduct LogObjectType = "product"
	// LogObjectPrice is a price key: its object id is product-key:price-key.
	LogObjectPrice LogObjectType = "price"
	LogObjectMeter LogObjectType = "meter"
	LogObjectLimit LogObjectType = "limit"
	// LogObjectMerchant is the merchant document: its object id is the
	// merchant's id.
	LogObjectMerchant LogObjectType = "merchant"
	// LogObjectMessages is the merchant's messages document: its object id
	// is the merchant's id.
	LogObjectMessages  LogObjectType = "messages"
	LogObjectPSP       LogObjectType = "psp"
	LogObjectCustodian LogObjectType = "custodian"
)

// LogAction is what happened to the object.
type LogAction string

const (
	LogActionCreate    LogAction = "create"
	LogActionUpdate    LogAction = "update"
	LogActionArchive   LogAction = "archive"
	LogActionUnarchive LogAction = "unarchive"
	LogActionDelete    LogAction = "delete"
	// LogActionKeyAdd, LogActionKeyRemove and LogActionKeyRename change a
	// product's entitlement keys.
	LogActionKeyAdd    LogAction = "key_add"
	LogActionKeyRemove LogAction = "key_remove"
	LogActionKeyRename LogAction = "key_rename"
	// LogActionMove moves a price key: from then it names another of its
	// prices, or, archived, none.
	LogActionMove LogAction = "move"
)

// LogEntry is one entry of a merchant's history. Changes is versioned per
// action: create, update, archive, unarchive and delete carry
// {"version":1,"fields":{field:{"before":…,"after":…}}} (a field absent
// before or after omits that side), a configuration document adds its
// "revision", and a credential field reads "changed"; key_add and key_remove
// carry {"version":1,"keys":[…]}, key_rename {"version":1,"from":…,"to":…},
// and a price's move {"version":1,"price_id":…,"archived":…} with its
// revision.
type LogEntry struct {
	ID          LogEntryID      `json:"id"`
	OccurredAt  time.Time       `json:"occurred_at"`
	Actor       LogActor        `json:"actor"`
	Source      LogSource       `json:"source"`
	ObjectType  LogObjectType   `json:"object_type"`
	ObjectID    string          `json:"object_id"`
	Action      LogAction       `json:"action"`
	Changes     json.RawMessage `json:"changes"`
	Reason      *string         `json:"reason"`
	RequestID   *string         `json:"request_id"`
	OperationID *string         `json:"operation_id"`
}

// LogListParams filters a history, newest first: one object's entries
// (ObjectID needs ObjectType), and those at or after Since and before Until.
type LogListParams struct {
	PageRequest
	ObjectType LogObjectType `form:"object_type"`
	ObjectID   string        `form:"object_id"`
	Since      time.Time     `form:"since"`
	Until      time.Time     `form:"until"`
}
