package openrailstest

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/helpers/userinfo"
	"github.com/open-rails/helpers/userinfo/userinfotest"
)

func TestUserInfoIsALookup(t *testing.T) {
	d := &UserInfo{}
	ada := userinfo.User{ID: uuid.NewString(), Email: "ada.lovelace@example.test", Name: "Ada Lovelace", Username: "countess_ada"}
	alan := userinfo.User{ID: uuid.NewString(), Email: "alan.turing@example.test", Name: "Alan Turing", Username: "enigma_alan"}
	d.Put(ada)
	d.Put(alan)
	userinfotest.Check(t, d, userinfotest.Fixtures{
		Users:   []userinfo.User{ada, alan},
		Unknown: []string{uuid.NewString()},
		Change: func(u userinfo.User) userinfo.User {
			u.Email, u.Name, u.Username = "changed."+u.Email, "Changed "+u.Name, "changed_"+u.Username
			d.Put(u)
			return u
		},
	})
}
