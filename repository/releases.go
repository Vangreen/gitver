package repository

import (
	"encoding/json"
	"fmt"
	"github.com/dgraph-io/badger/v4"
)

// StoreReleases saves the releases slice into Badger DB (serialized as JSON).
func StoreReleases(releases []Release, db *badger.DB) error {
	fmt.Println("store")
	data, err := json.Marshal(releases)
	if err != nil {
		return err
	}
	err = db.Update(func(txn *badger.Txn) error {
		return txn.Set([]byte("releases"), data)
	})
	return err
}

// GetReleasesFromDB loads the releases slice from Badger DB.
func GetReleasesFromDB(db *badger.DB) ([]Release, error) {
	var releases []Release
	err := db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte("releases"))
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			return json.Unmarshal(val, &releases)
		})
	})
	return releases, err
}
