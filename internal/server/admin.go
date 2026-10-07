package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// RecoverAccount helps an administrator who is locked out of the dashboard:
// it sets a new password and/or turns off two-factor sign-in. It runs on the
// server itself (`backupproof admin …`), so whoever runs it already controls
// the data folder.
func RecoverAccount(dataDir, username, newPassword string, resetTwoFactor bool) error {
	st, err := openExistingStore(dataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	var id int64
	if err := st.db.QueryRow("SELECT id FROM users WHERE username=?", username).Scan(&id); err != nil {
		return fmt.Errorf("no account named %q (run `backupproof admin users` to list them)", username)
	}
	if newPassword != "" {
		if err := st.SetPassword(id, newPassword); err != nil {
			return err
		}
		_ = st.Audit("server console", "reset-password", fmt.Sprintf("set a new password for %q", username))
	}
	if resetTwoFactor {
		if err := st.DisableTwoFactor(id); err != nil {
			return err
		}
		_ = st.Audit("server console", "reset-2fa", fmt.Sprintf("turned off two-factor sign-in for %q", username))
	}
	return nil
}

// ListAccounts returns the dashboard's accounts, for `backupproof admin users`.
func ListAccounts(dataDir string) ([]User, error) {
	st, err := openExistingStore(dataDir)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	return st.Users()
}

func openExistingStore(dataDir string) (*Store, error) {
	if _, err := os.Stat(filepath.Join(dataDir, "backupproof.db")); err != nil {
		return nil, fmt.Errorf("no BackupProof dashboard data in %s (use --data, e.g. /var/lib/backupproof)", dataDir)
	}
	if os.Getenv("BP_SECRET_KEY") == "" {
		if _, err := os.Stat(filepath.Join(dataDir, "secret.key")); err != nil {
			return nil, errors.New("secret.key is missing from the data folder (set BP_SECRET_KEY if the server uses it)")
		}
	}
	secret, err := loadSecret(dataDir)
	if err != nil {
		return nil, err
	}
	return OpenStore(dataDir, secret)
}
