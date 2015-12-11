/*
Licensed to the Apache Software Foundation (ASF) under one
or more contributor license agreements.  See the NOTICE file
distributed with this work for additional information
regarding copyright ownership.  The ASF licenses this file
to you under the Apache License, Version 2.0 (the
"License"); you may not use this file except in compliance
with the License.  You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing,
software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
KIND, either express or implied.  See the License for the
specific language governing permissions and limitations
under the License.
*/

package client

import (
	"database/sql"
	"fmt"
	_ "fmt"
	_ "github.com/mattn/go-sqlite3"
	"github.com/op/go-logging"
	"github.com/openblockchain/obc-peer/openchain/crypto/utils"
	"os"
	_ "os"
	"path/filepath"
	_ "path/filepath"
	"sync"
	_ "sync"
)

func (client *clientImpl) initKeyStore() error {
	// TODO: move all the ket/certificate store/load to the keyStore struct

	ks := keyStore{}
	ks.log = client.log
	ks.conf = client.conf
	if err := ks.Init(); err != nil {
		return err
	}

	client.ks = &ks

	return nil
}

type keyStore struct {
	isOpen bool

	// backend
	sqlkeystore *sql.DB

	// Configuration
	conf *configuration

	// Logging
	log *logging.Logger

	// Sync
	m sync.Mutex
}

func (ks *keyStore) Init() error {
	ks.m.Lock()
	defer ks.m.Unlock()

	if ks.isOpen {
		return utils.ErrKeyStoreAlreadyInitialized
	}

	err := ks.createKeyStoreIfKeyStorePathEmpty()
	if err != nil {
		return err
	}

	err = ks.openKeyStore()
	if err != nil {
		return err
	}

	return nil
}

func (ks *keyStore) GetNextTCert(tCertFetcher func(num int) ([][]byte, [][]byte, error)) ([]byte, []byte, error) {
	ks.m.Lock()
	defer ks.m.Unlock()

	cert, key, err := ks.selectNextTCert()
	if err != nil {
		ks.log.Error("Failed selecting next TCert: %s", err)

		return nil, nil, err
	}
	ks.log.Info("cert %s", utils.EncodeBase64(cert))

	if cert == nil {
		// If No TCert is available, fetch new ones, store them and return the first available.

		// 1. Fetch
		ks.log.Info("Fectch TCerts from TCA...")
		certs, keys, err := tCertFetcher(10)
		if err != nil {
			return nil, nil, err
		}

		// 2. Store
		ks.log.Info("Store them...")
		tx, err := ks.sqlkeystore.Begin()
		if err != nil {
			ks.log.Error("Failed beginning transaction: %s", err)

			return nil, nil, err
		}

		for i, cert := range certs {
			ks.log.Info("Insert index %d", i)

			//			db.log.Info("Insert key %s", utils.EncodeBase64(keys[i]))
			ks.log.Info("Insert cert %s", utils.EncodeBase64(cert))

			// TODO: once the TCert structure is finalized,
			// store only the cert from which the corresponding key
			// can be derived

			_, err := tx.Exec("INSERT INTO TCerts (cert, key) VALUES (?, ?)", cert, keys[i])

			if err != nil {
				ks.log.Error("Failed inserting cert %s", err)
				continue
			}
		}

		err = tx.Commit()
		if err != nil {
			ks.log.Error("Failed committing transaction: %s", err)

			tx.Rollback()

			return nil, nil, err
		}

		ks.log.Info("Fectch TCerts from TCA...done!")

		cert, key, err = ks.selectNextTCert()
		if err != nil {
			ks.log.Error("Failed selecting next TCert after fetching: %s", err)

			return nil, nil, err
		}
	}

	return cert, key, nil
	//	return nil, nil, errors.New("No cert obtained")
	//	return utils.NewSelfSignedCert()
}

func (ks *keyStore) Close() error {
	ks.log.Info("Closing keystore...")
	err := ks.sqlkeystore.Close()

	if err != nil {
		ks.log.Error("Failed closing keystore: %s", err)
	} else {
		ks.log.Info("Closing keystore...done!")
	}

	ks.isOpen = false
	return err
}

func (ks *keyStore) selectNextTCert() ([]byte, []byte, error) {
	ks.log.Info("Select next TCert...")

	// Open transaction
	tx, err := ks.sqlkeystore.Begin()
	if err != nil {
		ks.log.Error("Failed beginning transaction: %s", err)

		return nil, nil, err
	}

	// Get the first row available
	var id int
	var cert, key []byte
	row := ks.sqlkeystore.QueryRow("SELECT id, cert, key FROM TCerts")
	err = row.Scan(&id, &cert, &key)

	if err == sql.ErrNoRows {
		return nil, nil, nil
	} else if err != nil {
		ks.log.Error("Error during select: %s", err)

		return nil, nil, err
	}

	ks.log.Info("id %d", id)
	ks.log.Info("cert %s", utils.EncodeBase64(cert))
	//	db.log.Info("key %s", utils.EncodeBase64(key))

	// TODO: instead of removing, move the TCert to a new table
	// which stores the TCerts used

	// Remove that row
	ks.log.Info("Removing row with id [%d]...", id)

	if _, err := tx.Exec("DELETE FROM TCerts WHERE id = ?", id); err != nil {
		ks.log.Error("Failed removing row [%d]: %s", id, err)

		tx.Rollback()

		return nil, nil, err
	}

	ks.log.Info("Removing row with id [%d]...done", id)

	// Finalize
	err = tx.Commit()
	if err != nil {
		ks.log.Error("Failed commiting: %s", err)
		tx.Rollback()

		return nil, nil, err
	}

	ks.log.Info("Select next TCert...done!")

	return cert, key, nil
}

func (ks *keyStore) createKeyStoreIfKeyStorePathEmpty() error {
	// Check directory
	ksPath := ks.conf.getKeyStorePath()
	missing, err := utils.DirMissingOrEmpty(ksPath)
	if err != nil {
		ks.log.Error("Failed checking directory %s: %s", ksPath, err)
	}
	ks.log.Debug("Keystore path [%s] missing [%t]", ksPath, missing)

	if !missing {
		// Check file
		missing, err = utils.FileMissing(ks.conf.getKeyStorePath(), ks.conf.getKeyStoreFilename())
		if err != nil {
			ks.log.Error("Failed checking file %s: %s", ks.conf.getKeyStoreFilePath(), err)
		}
		ks.log.Debug("Keystore file [%s] missing [%t]", ks.conf.getKeyStoreFilePath(), missing)
	}

	if missing {
		err := ks.createKeyStore()
		if err != nil {
			ks.log.Debug("Failed creating db At [%s]: %s", ks.conf.getKeyStoreFilePath(), err.Error())
			return nil
		}
	}

	return nil
}

func (ks *keyStore) createKeyStore() error {
	dbPath := ks.conf.getKeyStorePath()
	ks.log.Debug("Creating keystore at [%s]", dbPath)

	missing, err := utils.FileMissing(dbPath, ks.conf.getKeyStoreFilename())
	if !missing {
		return fmt.Errorf("db dir [%s] already exists", dbPath)
	}

	os.MkdirAll(dbPath, 0755)

	ks.log.Debug("Open keystore at [%s]", dbPath)
	db, err := sql.Open("sqlite3", filepath.Join(dbPath, ks.conf.getKeyStoreFilename()))
	if err != nil {
		return err
	}

	ks.log.Debug("Ping keystore at [%s]", dbPath)
	err = db.Ping()
	if err != nil {
		ks.log.Fatal(err)
	}

	defer db.Close()

	// create tables
	ks.log.Debug("Create Table [%s] at [%s]", "TCert", dbPath)
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS TCerts (id INTEGER, cert BLOB, key BLOB, PRIMARY KEY (id))"); err != nil {
		ks.log.Debug("Failed creating table: %s", err)
		return err
	}

	ks.log.Debug("keystore created at [%s]", dbPath)
	return nil
}

func (ks *keyStore) deleteKeyStore() error {
	ks.log.Debug("Removing KeyStore at [%s]", ks.conf.getKeyStorePath())

	return os.RemoveAll(ks.conf.getKeyStorePath())
}

func (ks *keyStore) openKeyStore() error {
	if ks.isOpen {
		return nil
	}
	ksPath := ks.conf.getKeyStorePath()

	sqlDB, err := sql.Open("sqlite3", filepath.Join(ksPath, ks.conf.getKeyStoreFilename()))

	if err != nil {
		ks.log.Error("Error opening keystore", err)
		return err
	}
	ks.isOpen = true
	ks.sqlkeystore = sqlDB

	return nil
}
