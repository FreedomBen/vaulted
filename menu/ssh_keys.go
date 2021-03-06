package menu

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"os"
	"os/user"
	"path/filepath"
	"sort"

	"github.com/fatih/color"
	"github.com/miquella/ask"
	"golang.org/x/crypto/ssh"
)

type SSHKeyMenu struct {
	Menu
}

func (m *SSHKeyMenu) Help() {
	menuColor.Set()
	defer color.Unset()

	fmt.Println("a - Add")
	fmt.Println("D - Delete")
	fmt.Println("? - Help")
	fmt.Println("b - Back")
	fmt.Println("q - Quit")
}

func (m *SSHKeyMenu) Handler() error {
	for {
		var err error
		m.Printer()
		input, err := interaction.ReadMenu("Edit ssh keys: [a,D,b]: ")
		if err != nil {
			return err
		}
		switch input {
		case "a", "add", "key", "keys":
			err = m.AddSSHKey()
		case "D", "delete", "remove":
			var key string
			key, err = interaction.ReadValue("Key: ")
			if err == nil {
				if _, exists := m.Vault.SSHKeys[key]; exists {
					delete(m.Vault.SSHKeys, key)
				} else {
					color.Red("Key '%s' not found", key)
				}
			}
		case "b", "back":
			return nil
		case "q", "quit", "exit":
			var confirm string
			confirm, err = interaction.ReadValue("Are you sure you wish to save and exit the vault? (y/n): ")
			if err == nil {
				if confirm == "y" {
					return ErrSaveAndExit
				}
			}
		case "?", "help":
			m.Help()
		default:
			color.Red("Command not recognized")
		}

		if err != nil {
			return err
		}
	}
}

func (m *SSHKeyMenu) AddSSHKey() error {
	var err error

	homeDir := ""
	user, err := user.Current()
	if err == nil {
		homeDir = user.HomeDir
	} else {
		homeDir = os.Getenv("HOME")
	}

	defaultFilename := ""
	filename := ""
	if homeDir != "" {
		defaultFilename = filepath.Join(homeDir, ".ssh", "id_rsa")
		filename, err = interaction.ReadValue(fmt.Sprintf("Key file (default: %s): ", defaultFilename))
		if err != nil {
			return err
		}
		if filename == "" {
			filename = defaultFilename
		}
		if !filepath.IsAbs(filename) {
			filename = filepath.Join(filepath.Join(homeDir, ".ssh"), filename)
		}
	} else {
		filename, err = interaction.ReadValue("Key file: ")
		if err != nil {
			return err
		}
	}

	decryptedBlock, err := loadAndDecryptKey(filename)
	if err != nil {
		color.Red("%v", err)
		return nil
	}

	comment := loadPublicKeyComment(filename + ".pub")
	var name string
	if comment != "" {
		name, err = interaction.ReadValue(fmt.Sprintf("Name (default: %s): ", comment))
		if err != nil {
			return err
		}
		if name == "" {
			name = comment
		}
	} else {
		name, err = interaction.ReadValue("Name: ")
		if err != nil {
			return err
		}
		if name == "" {
			name = filename
		}
	}

	if m.Vault.SSHKeys == nil {
		m.Vault.SSHKeys = make(map[string]string)
	}
	m.Vault.SSHKeys[name] = string(pem.EncodeToMemory(decryptedBlock))

	return nil
}

func loadAndDecryptKey(filename string) (*pem.Block, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	data, err := ioutil.ReadAll(f)
	if err != nil {
		return nil, err
	}

	block, _ := pem.Decode(data)
	if block == nil {
		return nil, err
	}

	if x509.IsEncryptedPEMBlock(block) {
		var passphrase string
		var decryptedBytes []byte
		for i := 0; i < 3; i++ {
			passphrase, err = ask.HiddenAsk("Passphrase: ")
			if err != nil {
				return nil, err
			}

			decryptedBytes, err = x509.DecryptPEMBlock(block, []byte(passphrase))
			if err == nil {
				break
			}
			if err != x509.IncorrectPasswordError {
				return nil, err
			}
		}

		if err != nil {
			return nil, err
		}

		return &pem.Block{
			Type:  block.Type,
			Bytes: decryptedBytes,
		}, nil
	}
	return block, nil
}

func loadPublicKeyComment(filename string) string {
	f, err := os.Open(filename)
	if err != nil {
		return ""
	}
	defer f.Close()

	data, err := ioutil.ReadAll(f)
	if err != nil {
		return ""
	}

	_, comment, _, _, err := ssh.ParseAuthorizedKey(data)
	if err != nil {
		return ""
	}
	return comment
}

func (m *SSHKeyMenu) Printer() {
	color.Cyan("\nSSH Keys:")
	if len(m.Vault.SSHKeys) > 0 {
		keys := []string{}
		for key := range m.Vault.SSHKeys {
			keys = append(keys, key)
		}
		sort.Strings(keys)

		for _, key := range keys {
			green.Printf("  %s\n", key)
		}
	} else {
		fmt.Println("  [Empty]")
	}
}
