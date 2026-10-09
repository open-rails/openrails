package openrailstest

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/helpers/contacts/contactstest"

	"github.com/open-rails/openrails"
)

func TestContactsIsASource(t *testing.T) {
	d := &Contacts{}
	ada := openrails.Contact{ID: uuid.NewString(), Email: "ada.lovelace@example.test", Name: "Ada Lovelace", Username: "countess_ada"}
	alan := openrails.Contact{ID: uuid.NewString(), Email: "alan.turing@example.test", Name: "Alan Turing", Username: "enigma_alan"}
	d.Put(ada)
	d.Put(alan)
	contactstest.Check(t, d, contactstest.Fixtures{
		Contacts: []openrails.Contact{ada, alan},
		Unknown:  []string{uuid.NewString()},
		Change: func(c openrails.Contact) openrails.Contact {
			c.Email, c.Name, c.Username = "changed."+c.Email, "Changed "+c.Name, "changed_"+c.Username
			d.Put(c)
			return c
		},
	})
}
