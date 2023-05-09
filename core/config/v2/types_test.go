package v2

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/smartcontractkit/chainlink/v2/core/services/chainlink/cfgtest"
	"github.com/smartcontractkit/chainlink/v2/core/store/models"
	"github.com/smartcontractkit/chainlink/v2/core/utils"
)

func TestCoreDefaults_notNil(t *testing.T) {
	cfgtest.AssertFieldsNotNil(t, &defaults)
}

func TestMercurySecrets_valid(t *testing.T) {
	ms := MercurySecrets{
		Credentials: map[string]MercuryCredentials{
			"cred1": {
				URL:      models.MustSecretURL("https://facebook.com"),
				Username: models.NewSecret("new user1"),
				Password: models.NewSecret("new password1"),
			},
			"cred2": {
				URL:      models.MustSecretURL("HTTPS://GOOGLE.COM"),
				Username: models.NewSecret("new user1"),
				Password: models.NewSecret("new password2"),
			},
		},
	}

	err := ms.ValidateConfig()
	assert.NoError(t, err)
}

func TestMercurySecrets_duplicateURLs(t *testing.T) {
	ms := MercurySecrets{
		Credentials: map[string]MercuryCredentials{
			"cred1": {
				URL:      models.MustSecretURL("HTTPS://GOOGLE.COM"),
				Username: models.NewSecret("new user1"),
				Password: models.NewSecret("new password1"),
			},
			"cred2": {
				URL:      models.MustSecretURL("HTTPS://GOOGLE.COM"),
				Username: models.NewSecret("new user2"),
				Password: models.NewSecret("new password2"),
			},
		},
	}

	err := ms.ValidateConfig()
	assert.Error(t, err)
	assert.Equal(t, "URL: invalid value (https://GOOGLE.COM): duplicate - must be unique", err.Error())
}

func TestMercurySecrets_emptyURL(t *testing.T) {
	ms := MercurySecrets{
		Credentials: map[string]MercuryCredentials{
			"cred1": {
				URL:      nil,
				Username: models.NewSecret("new user1"),
				Password: models.NewSecret("new password1"),
			},
		},
	}

	err := ms.ValidateConfig()
	assert.Error(t, err)
	assert.Equal(t, "URL: missing: must be provided and non-empty", err.Error())
}

func TestLogFile_createsDirectory(t *testing.T) {
	newDir := filepath.Join(os.TempDir(), uuid.New().String())
	maxSize := utils.FileSize(100) * utils.MB

	lg := LogFile{
		Dir:     &newDir,
		MaxSize: &maxSize,
	}

	assert.NoError(t, lg.ValidateConfig())
	fi, err := os.Stat(newDir)
	assert.NoError(t, err)
	assert.True(t, fi.IsDir())

	os.RemoveAll(newDir)
}

func TestLogFile_errorsIfCantCreateDirectory(t *testing.T) {
	// Try to create a file in the root where we shouldn't have permissions.
	newDir := "/" + uuid.New().String()
	maxSize := utils.FileSize(100) * utils.MB

	lg := LogFile{
		Dir:     &newDir,
		MaxSize: &maxSize,
	}

	assert.ErrorContains(t, lg.ValidateConfig(), fmt.Sprintf("invalid value (%s)", newDir))
}

func TestLogFile_noValidationIfDiskLoggingDisabled(t *testing.T) {
	// Try to create a file in the root where we shouldn't have permissions.
	newDir := "/" + uuid.New().String()
	maxSize := utils.FileSize(0)

	lg := LogFile{
		Dir:     &newDir,
		MaxSize: &maxSize,
	}

	assert.NoError(t, lg.ValidateConfig())

	_, err := os.Stat(newDir)
	assert.ErrorIs(t, err, fs.ErrNotExist)

}
