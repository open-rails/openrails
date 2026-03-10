package payments

type UserIdentity struct {
	ID       string
	Email    *string
	Username string
	Roles    []string
}
