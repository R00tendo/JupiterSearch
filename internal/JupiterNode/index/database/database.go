package database

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

var (
	BadgerDB *badger.DB

	ConcurrencyLock = make(chan any, 100)

	ConcurrentlyRunning ConcurrentlyRunningStruct

	uuidRegex = regexp.MustCompile(`^[0-9a-fA-F]{8}\b-[0-9a-fA-F]{4}\b-[0-9a-fA-F]{4}\b-[0-9a-fA-F]{4}\b-[0-9a-fA-F]{12}$`)
)

type ConcurrentlyRunningStruct struct {
	Amount int
}

func Stop() {
	if BadgerDB != nil {
		BadgerDB.Close()
	}
}

func Init() error {
	var err error

	BadgerDB, err = badger.Open(badger.DefaultOptions(viper.GetString("datadir")))
	if err != nil {
		return err
	}

	logrus.Info("database ok")
	return nil
}

func Store(tokens map[string][]string, original map[string]any) (string, error) {
	ConcurrentlyRunning.Wait()
	ConcurrentlyRunning.Add()
	defer ConcurrentlyRunning.Done()

	logrus.Debug("ingesting data")

	ID := uuid.New()

	jsonDoc, err := json.Marshal(original)

	if err != nil {
		return "", err
	}

	txn := BadgerDB.NewTransaction(true)
	defer txn.Discard()

	txn.Set([]byte(ID.String()), jsonDoc)
	txn.Commit()
	for _, tokenStrings := range tokens {
		for _, token := range tokenStrings {
			merger := BadgerDB.GetMergeOperator([]byte(token), addVal, 100*time.Millisecond)
			err := merger.Add([]byte(ID.String()))
			if err != nil {
				logrus.Error(err.Error())
				break
			}
			merger.Stop()
		}
	}

	err = BadgerDB.Sync()
	if err != nil {
		logrus.Error(err.Error())
		return "", err
	}

	logrus.Debug("ingest done")

	return ID.String(), nil
}

func Retrieve(query string) (string, error) {
	var results string
	var doc string

	queries := strings.Split(strings.ToLower(strings.TrimSpace(query)), " ")
	for _, query := range queries {
		if uuidRegex.Match([]byte(query)) {
			doc = query
			break
		}
	}

	var err error
	docMatches := make(map[string]int)

	if doc != "" {
		err = BadgerDB.View(func(txn *badger.Txn) error {
			tempResults, err := txn.Get([]byte(query))
			if err != nil {
				return err
			}

			tempResults.Value(func(byteResults []byte) error {
				results = string(byteResults)
				return nil
			})

			return nil
		})
	} else {
		for _, query := range queries {
			err = BadgerDB.View(func(txn *badger.Txn) error {
				tempResults, err := txn.Get([]byte(query))
				if err != nil {
					return err
				}

				tempResults.Value(func(byteResults []byte) error {
					for _, docID := range strings.Split(string(byteResults), ":") {
						docMatches[docID]++
					}
					return nil
				})

				return nil
			})
			if err != nil {
				return "", err
			}
		}

		var tempResults []string

		for docID, matches := range docMatches {
			if matches == len(queries) {
				tempResults = append(tempResults, docID)
			}
		}
		results = strings.Join(tempResults, ":")
	}
	return results, err
}

func addVal(originalValue, newValue []byte) []byte {
	return append(originalValue, append([]byte(":"), newValue...)...)
}

func DirSize() (float64, error) {
	var size int64

	err := filepath.Walk(viper.GetString("datadir"), func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() {
			size += info.Size()
		}

		return nil
	})

	return float64(size), err
}

func (concurrentlyRunning *ConcurrentlyRunningStruct) Wait() {
	for concurrentlyRunning.Amount >= viper.GetInt("max_concurrent_ingests") {
		<-ConcurrencyLock
	}
}

func (concurrentlyRunning *ConcurrentlyRunningStruct) Add() {
	concurrentlyRunning.Amount++
	ConcurrencyLock <- true
}

func (concurrentlyRunning *ConcurrentlyRunningStruct) Done() {
	concurrentlyRunning.Amount--
	ConcurrencyLock <- true
}
