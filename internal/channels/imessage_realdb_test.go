package channels

import (
	"database/sql"
	"os"
	"testing"
)

// Opt-in check against the developer's real chat.db (needs Full Disk
// Access). Reports decode agreement only — never prints message content.
//   FASTCLAW_IMESSAGE_REALDB=1 go test ./internal/channels -run RealChatDB -v
func TestIMessageRealChatDB(t *testing.T) {
	if os.Getenv("FASTCLAW_IMESSAGE_REALDB") == "" {
		t.Skip("set FASTCLAW_IMESSAGE_REALDB=1 to run")
	}
	db, err := openIMessageDB(IMessageChatDBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT text, attributedBody FROM message WHERE attributedBody IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var total, agree, withText, nullTextDecoded, nullText int
	for rows.Next() {
		var text sql.NullString
		var body []byte
		if err := rows.Scan(&text, &body); err != nil {
			t.Fatal(err)
		}
		total++
		got := decodeAttributedBody(body)
		if text.Valid && text.String != "" {
			withText++
			if got == text.String {
				agree++
			}
		} else {
			nullText++
			if got != "" {
				nullTextDecoded++
			}
		}
	}
	t.Logf("rows=%d; text present: %d/%d decoded identically; text NULL: %d/%d decoded non-empty",
		total, agree, withText, nullTextDecoded, nullText)
	if _, err := (&IMessage{}).poll(t.Context(), db, 1<<62); err != nil {
		t.Fatalf("poll query against real schema: %v", err)
	}
}
