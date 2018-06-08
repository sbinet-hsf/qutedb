/*
The MIT License

Copyright (c) 2018 Maurizio Tomasi

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/

package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/elithrar/simple-scrypt"
	"github.com/gobuffalo/uuid"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

// An User is somebody which can have (read-only) access to the data
type User struct {
	gorm.Model
	Email          string `gorm:"unique_index"`
	HashedPassword []byte
	Superuser      bool
}

// A Session records who is currently allowed to access the site. This only
// happens if a user has successfully logged in.
type Session struct {
	gorm.Model
	UUID   string `gorm:"size:36;unique_index"`
	UserID uint
}

// An AsicDataFile represents the file containing either raw or sum data
// acquired with one ASIC
type AsicDataFile struct {
	ID       int `gorm:"primary_key"`
	FileName string
}

// An Acquisition represents a set of files within a folder in the repository
type Acquisition struct {
	gorm.Model

	Directoryname      string `gorm:"unique_index"`
	CreationTime       time.Time
	RawFiles           []AsicDataFile
	SumFiles           []AsicDataFile
	AsicHkFileName     string
	ExternHkFileName   string
	CryostatHkFileName string
}

// InitDb creates all the tables in the database. It takes care
// of not raising errors if the tables are already present.
func InitDb(db *gorm.DB, config *Configuration) error {
	db.AutoMigrate(&User{}, &Session{}, &AsicDataFile{}, &Acquisition{})

	// Clear all existing sessions in the database. Ignore any error
	db.Delete(&Session{})

	// Refresh the contents of the database
	return RefreshDbContents(db, config.RepositoryPath)
}

// HkDirName returns the name of the test directory containing the housekeeping files
func HkDirName(folder string) string {
	return path.Join(folder, "Hks")
}

// RawDirName returns the name of the test directory containing raw files
func RawDirName(folder string) string {
	return path.Join(folder, "Raws")
}

// SumDirName returns the name of the test directory containing raw files
func SumDirName(folder string) string {
	return path.Join(folder, "Sums")
}

func listFilesInDir(dirpath string) []string {
	result := []string{}

	files, err := ioutil.ReadDir(dirpath)
	if err != nil {
		return result
	}

	for _, f := range files {
		if f.IsDir() || strings.ToLower(path.Ext(f.Name())) != ".fits" {
			continue
		}
		result = append(result, f.Name())
	}

	return result
}

func findMultipleFiles(path string, mask string) ([]string, error) {
	filenames, err := filepath.Glob(filepath.Join(path, mask))
	if err != nil || len(filenames) == 0 {
		return []string{}, err
	}

	result := []string{}
	for _, curname := range filenames {
		fi, err := os.Stat(curname)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}

		result = append(result, curname)
	}

	return result, nil
}

// findOneMatchingFile looks for the files matching "mask" (a file name pattern
// built using POSIX wildcards). If no matches are found, it returns "". If one
// match is found, it returns the name of the file. If more tha one match is
// found, it returns an error. matching files were found.
func findOneMatchingFile(path string, mask string) (string, error) {
	filenames, err := filepath.Glob(filepath.Join(path, mask))
	if err != nil || len(filenames) == 0 {
		return "", fmt.Errorf("Error in \"glob\": %s", err)
	}

	if len(filenames) > 1 {
		return "", fmt.Errorf("Found more than one file (%d) matching the mask \"%s\"",
			len(filenames), mask)
	}

	fi, err := os.Stat(filenames[0])
	if err != nil {
		return "", fmt.Errorf("Unable to retrieve information for \"%s\": %s", filenames[0], err)
	}

	if !fi.Mode().IsRegular() {
		return "", nil
	}

	return filenames[0], nil
}

// refreshFolder scans a folder containing *one* acquisition and updates the
// database accordingly. This function does not check whether "folderPath" is
// really within the repository or not.
func refreshFolder(db *gorm.DB, folderPath string) error {
	newacq := Acquisition{
		Directoryname: filepath.Base(folderPath),
	}

	// Check if the folder is already present in the db
	result := db.Where("directoryname = ?", newacq.Directoryname).First(&Acquisition{})
	if !result.RecordNotFound() {
		return nil
	}

	// Check for the presence of housekeeping files
	hkDir := HkDirName(folderPath)
	if filename, err := findOneMatchingFile(hkDir, "conf-asics-????.??.??.??????.fits"); err == nil && filename != "" {
		newacq.AsicHkFileName = filename
	}
	if filename, err := findOneMatchingFile(hkDir, "hk-extern-????.??.??.??????.fits"); err == nil && filename != "" {
		newacq.ExternHkFileName = filename
	}
	// TODO: Cryostat thermometers will need to be considered at this point,
	// once the mask for their files is finalized

	if rawFiles, err := findMultipleFiles(RawDirName(folderPath), "raw-asic*-????.??.??.??????.fits"); err == nil && len(rawFiles) > 0 {
		for _, filename := range rawFiles {
			datafile := AsicDataFile{FileName: filename}
			db.Create(&datafile)
			newacq.RawFiles = append(newacq.RawFiles, datafile)
		}
	}

	if sumFiles, err := findMultipleFiles(SumDirName(folderPath), "science-asic*-????.??.??.??????.fits"); err == nil && len(sumFiles) > 0 {
		for _, filename := range sumFiles {
			datafile := AsicDataFile{FileName: filename}
			db.Create(&datafile)
			newacq.SumFiles = append(newacq.SumFiles, datafile)
		}
	}

	if db.Create(&newacq).Error != nil {
		return fmt.Errorf("Error while creating a new acquisition for \"%s\": %s",
			folderPath, db.Error)
	}

	return nil
}

// RefreshDbContents scans the repository for any file that is missing from the
// database, and create an entry for each of them
func RefreshDbContents(db *gorm.DB, repositoryPath string) error {
	folders, err := filepath.Glob(filepath.Join(repositoryPath, "????-??-??_??.??.??__*"))
	if err != nil {
		return err
	}
	for _, curfolder := range folders {
		if fi, err := os.Stat(curfolder); err != nil || !fi.Mode().IsDir() {
			// Skip entries that are not real directories
			continue
		}

		if err := refreshFolder(db, curfolder); err != nil {
			return err
		}
	}

	return nil
}

// CreateUser creates a new "User" object and initializes it with the hash of
// the password and the other parameters as well. The new object is saved in the
// database.
func CreateUser(db *gorm.DB, email string, password string, superuser bool) (*User, error) {
	hash, err := scrypt.GenerateFromPassword([]byte(password), scrypt.DefaultParams)
	if err != nil {
		return nil, err
	}

	user := User{Email: email, HashedPassword: hash, Superuser: superuser}
	if err := db.Create(&user).Error; err != nil {
		return nil, err
	}

	return &user, nil
}

// DeleteUser removes an user from the database
func DeleteUser(db *gorm.DB, user *User) error {
	// Use Unscoped to avoid soft deletions
	return db.Unscoped().Delete(user).Error
}

// QueryUserByEmail searches in the database for an user with the matching email
// and returns a poitner to a User structure. If the user is not found, the
// pointer is nil. The "error" variable is set to something else than nil only
// if a real error is occurred.
func QueryUserByEmail(db *gorm.DB, email string) (*User, error) {
	var user User
	result := db.Where("Email = ?", email).First(&user)

	if result.RecordNotFound() {
		return nil, nil
	}

	if result.Error != nil {
		return nil, result.Error
	}

	return &user, nil
}

// CheckUserPassword checks that an user with the specified email and password is
// in the database. If found, return the tuple (userID, true, nil). If the user
// does not exist, or if the passwords don't match, return (0, false, nil). In
// the event of an error, the last element in the returned tuple identifies the
// error.
func CheckUserPassword(db *gorm.DB, email string, password string) (uint, bool, error) {
	var user User
	if db.Where("Email = ?", email).First(&user).RecordNotFound() {
		return 0, false, nil
	} else if db.Error != nil {
		return 0, false, db.Error
	}

	if scrypt.CompareHashAndPassword(user.HashedPassword, []byte(password)) != nil {
		return 0, false, nil
	}

	return user.ID, true, nil
}

// CreateSession inserts a new "Session" object in the database. The object is
// uniquely identified by its UUID.
func CreateSession(db *gorm.DB, user *User) (*Session, error) {
	// If the user already has an active session, return it
	var existingSessions []Session
	if err := db.Model(user).Related(&existingSessions).Error; err != nil {
		return nil, err
	}

	if len(existingSessions) > 0 {
		return &existingSessions[0], nil
	}

	newUUID, err := uuid.NewV4()
	if err != nil {
		return nil, err
	}

	newSession := Session{UUID: newUUID.String(), UserID: user.ID}
	if err := db.Create(&newSession).Error; err != nil {
		return nil, err
	}

	return &newSession, nil
}

// DeleteSession finds a session with a matching UUID in the database and
// deletes it.
func DeleteSession(db *gorm.DB, UUID string) error {
	if err := db.Delete(Session{}, "UUID = ?", UUID).Error; err != nil {
		return err
	}

	return nil
}
