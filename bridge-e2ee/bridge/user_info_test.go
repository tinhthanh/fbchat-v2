package bridge

import (
	"testing"

	"go.mau.fi/mautrix-meta/pkg/messagix/table"
)

func TestContactInfoFromTable(t *testing.T) {
	if contactInfoFromTable(nil, 1) != nil {
		t.Fatal("nil table returned a contact")
	}
	tbl := &table.LSTable{
		LSDeleteThenInsertContact: []*table.LSDeleteThenInsertContact{{Id: 2, Name: "Bạn bè"}},
		LSVerifyContactRowExists: []*table.LSVerifyContactRowExists{
			{ContactId: 3, Name: ""},
			{ContactId: 4, Name: "Người lạ", ProfilePictureUrl: "https://scontent.example/p.jpg"},
		},
	}
	if got := contactInfoFromTable(tbl, 2); got == nil || got.Name != "Bạn bè" {
		t.Fatalf("contact row = %+v", got)
	}
	if got := contactInfoFromTable(tbl, 4); got == nil || got.Name != "Người lạ" || got.ProfilePictureUrl == "" {
		t.Fatalf("verify row = %+v", got)
	}
	if got := contactInfoFromTable(tbl, 3); got != nil {
		t.Fatalf("nameless row = %+v", got)
	}
	if got := contactInfoFromTable(tbl, 9); got != nil {
		t.Fatalf("unknown id = %+v", got)
	}
}
