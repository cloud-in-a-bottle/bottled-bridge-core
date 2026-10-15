package store

import "strings"

// BotRemoteID is the person that authors the bridge's own notices.
const BotRemoteID = "bot"

// NamePrefix starts the remote ID of a person known only by name, from an imported export.
const NamePrefix = "name:"

// IsNetworkPerson reports whether a remote ID is someone on the network, who can be messaged, rather than the bridge
// itself or a person known only by name.
func IsNetworkPerson(remoteID string) bool {
	return remoteID != "" && remoteID != BotRemoteID && !strings.HasPrefix(remoteID, NamePrefix)
}

// Person returns the row ID for a remote user, creating it if needed.
func (tx *Tx) Person(remoteID string) (int64, error) {
	var id int64
	err := tx.queryRow(
		"INSERT INTO people (remote_id, updated_at) VALUES (?, ?) "+
			"ON CONFLICT (remote_id) DO UPDATE SET remote_id = remote_id RETURNING id",
		remoteID, nowMS(),
	).Scan(&id)
	return id, err
}

func (tx *Tx) SetSelf(remoteID string) error {
	if _, err := tx.Person(remoteID); err != nil {
		return err
	}
	_, err := tx.exec("UPDATE people SET is_self = (remote_id = ?)", remoteID)
	return err
}

func (tx *Tx) SelfPerson() (int64, error) {
	var id int64
	err := tx.queryRow("SELECT id FROM people WHERE is_self").Scan(&id)
	return id, err
}

func (tx *Tx) SetPersonName(remoteID, name string) error {
	_, err := tx.exec(
		"INSERT INTO people (remote_id, name, updated_at) VALUES (?, ?, ?) "+
			"ON CONFLICT (remote_id) DO UPDATE SET name = excluded.name, updated_at = excluded.updated_at",
		remoteID, nullString(name), nowMS(),
	)
	return err
}

func (tx *Tx) SetPersonAvatar(remoteID, sha string) error {
	_, err := tx.exec(
		"INSERT INTO people (remote_id, avatar_sha256, updated_at) VALUES (?, ?, ?) "+
			"ON CONFLICT (remote_id) DO UPDATE SET avatar_sha256 = excluded.avatar_sha256, updated_at = excluded.updated_at",
		remoteID, nullString(sha), nowMS(),
	)
	return err
}

func (tx *Tx) PersonName(personID int64) (string, error) {
	var name string
	err := tx.queryRow("SELECT coalesce(name, '') FROM people WHERE id = ?", personID).Scan(&name)
	return name, err
}
