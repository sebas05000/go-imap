package commands

import (
	"github.com/sebas05000/go-imap"
)

// An UNSELECT command.
// See RFC 3691 section 2.
type Unselect struct{}

func (cmd *Unselect) Command() *imap.Command {
	return &imap.Command{Name: "UNSELECT"}
}

func (cmd *Unselect) Parse(fields []interface{}) error {
	return nil
}
