package resource

import (
	"github.com/artpar/go-imap"
	"github.com/artpar/go-imap/backend/backendutil"
	daptinid "github.com/daptin/daptin/server/id"
)

// fetchItemsContain reports whether the requested items include the named
// atom (e.g. "MODSEQ"), expanding macros/groups.
func fetchItemsContain(items []imap.FetchItem, name string) bool {
	for _, item := range items {
		for _, expanded := range item.Expand() {
			if expanded == imap.FetchItem(name) {
				return true
			}
		}
	}
	return false
}

// HighestModSequence returns the mailbox's persistent highestmodseq.
func (dimb *DaptinImapMailBox) HighestModSequence() (int64, error) {
	return dimb.dbResource["mail_box"].GetMailBoxHighestModSeq(dimb.mailBoxId, nil)
}

// VanishedSince returns the UID set of messages whose deletion was recorded at
// modseq strictly greater than since, optionally restricted to knownUIDs. The
// boolean is true only when deletion history is complete for the window:
// the mailbox watermark must be initialized and not exceed since.
func (dimb *DaptinImapMailBox) VanishedSince(since int64, knownUIDs *imap.SeqSet) (*imap.SeqSet, bool, error) {
	mailBoxCrud := dimb.dbResource["mail_box"]

	transaction, err := mailBoxCrud.Connection().Beginx()
	if err != nil {
		return nil, false, err
	}
	defer transaction.Rollback()

	watermark, initialized, err := mailBoxCrud.GetDeletionHistoryWatermark(dimb.mailBoxId, transaction)
	if err != nil {
		return nil, false, err
	}

	var set *imap.SeqSet
	if knownUIDs != nil {
		set, err = dimb.dbResource["mail"].DeletionUIDsInSet(dimb.mailBoxId, since, knownUIDs, transaction)
	} else {
		set, err = dimb.dbResource["mail"].DeletionUIDsSince(dimb.mailBoxId, since, transaction)
	}
	if err != nil {
		return nil, false, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, false, err
	}

	sufficient := initialized && watermark <= since
	return set, sufficient, nil
}

// ConditionalStoreFlags implements STORE/UID STORE with the UNCHANGEDSINCE
// modifier. Messages with modseq greater than unchangedSince are not modified
// and returned in the modified SeqSet (UIDs when uid is true, sequence numbers
// otherwise). In-sync messages are updated; messages whose resulting flags
// equal their current flags produce no change.
func (dimb *DaptinImapMailBox) ConditionalStoreFlags(uid bool, seqset *imap.SeqSet,
	operation imap.FlagsOp, flags []string, unchangedSince int64) (modified *imap.SeqSet, succeeded *imap.SeqSet, err error) {
	mailCrud := dimb.dbResource["mail"]

	transaction, err := mailCrud.Connection().Beginx()
	if err != nil {
		return nil, nil, err
	}
	defer transaction.Rollback()

	selections, err := mailCrud.SelectMailBoxCandidates(dimb.mailBoxId, uid, seqset, transaction)
	if err != nil {
		return nil, nil, err
	}
	mails, err := dimb.readSelectedMails(selections, false, transaction)
	if err != nil {
		return nil, nil, err
	}

	modified = &imap.SeqSet{}
	succeeded = &imap.SeqSet{}
	updated := map[daptinid.DaptinReferenceId]bool{}

	addResult := func(set *imap.SeqSet, value uint32) {
		set.AddNum(value)
	}

	for _, selection := range selections {
		// A message selected more than once must not fail after its first
		// successful update.
		if updated[selection.ReferenceID] {
			continue
		}
		mailRow, ok := mails[selection.ReferenceID]
		if !ok {
			continue
		}
		messageModSeq, _ := internalIdFromValue(mailRow["modseq"])
		if messageModSeq > unchangedSince {
			if uid {
				addResult(modified, selection.UID)
			} else {
				addResult(modified, selection.SequenceNumber)
			}
			continue
		}

		currentFlags, _ := mailRow["flags"].(string)
		newFlags := backendutil.UpdateFlags(FlagsFromStoredString(currentFlags), operation, flags)
		if FlagsEqual(FlagsFromStoredString(currentFlags), newFlags) {
			continue // no actual flag change: no new modseq
		}

		if err := mailCrud.UpdateMailFlags(dimb.mailBoxId, selection.ReferenceID, newFlags,
			dimb.sessionUser, transaction); err != nil {
			return nil, nil, err
		}
		updated[selection.ReferenceID] = true
		if uid {
			addResult(succeeded, selection.UID)
		} else {
			addResult(succeeded, selection.SequenceNumber)
		}
	}

	if err := transaction.Commit(); err != nil {
		return nil, nil, err
	}
	return modified, succeeded, nil
}
