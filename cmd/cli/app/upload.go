/*
Copyright © 2020 Luke Hinds <lhinds@redhat.com>

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/trillian"
	"github.com/projectrekor/rekor/pkg"
	"github.com/projectrekor/rekor/pkg/log"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
)

type RespStatusCode struct {
	Code string `json:"file_recieved"`
}

type getLeafResponse struct {
	Status RespStatusCode
	Leaf   *trillian.GetLeavesByIndexResponse
	Key    []byte
}

type RekorEntry struct {
	SHA       string `json:"SHA,omitempty"`
	URL       string `json:"URL,omitempty"`
	Signature []byte `json:"Signature"`
	PublicKey []byte `json:"PublicKey"`
}

type RekorArmorEntry struct {
	SHA       string `json:"SHA,omitempty"`
	URL       string `json:"URL,omitempty"`
	Signature string `json:"Signature"`
	PublicKey string `json:"PublicKey"`
}

func isArmorProtected(f *os.File) bool {
	log := log.Logger
	_, decodeErr := armor.Decode(f)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		log.Error("Error processing file:", err)
	}
	return decodeErr == nil
}

func hashGenerator(artifact string, fileObject []byte) string {
	log := log.Logger
	hasher := sha256.New()
	if strings.HasSuffix(artifact, ".gz") {
		log.Info("gzipped content detected")
		gz, err := gzip.NewReader(bytes.NewReader(fileObject))
		if err != nil {
			log.Error("Error:", err)
		}
		if _, err := io.Copy(hasher, gz); err != nil {
			log.Error("Error:", err)
		}
	} else {
		if _, err := hasher.Write(fileObject); err != nil {
			log.Error("Error:", err)
		}
	}
	sha := hex.EncodeToString(hasher.Sum(nil))
	return sha
}

// uploadCmd represents the upload command
var uploadCmd = &cobra.Command{
	Use:   "upload",
	Short: "Upload a rekord file",
	Long: `This command takes the public key, signature and URL
of the release artifact and uploads it to the rekor server.`,
	Run: func(cmd *cobra.Command, args []string) {
		log := log.Logger
		rekorServer := viper.GetString("rekor_server")
		url := rekorServer + "/api/v1/add"
		signature := viper.GetString("signature")
		publicKey := viper.GetString("public-key")
		artifactURL := viper.GetString("artifact-url")

		// Before we download anything or validate the signing
		// Let's check the formatting is correct, if not we
		// exit and allow the user to resolve their corrupted
		// GPG files.
		sig, err := pkg.FormatSignature(signature)
		if err != nil {
			log.Fatal("Signature validation failed: ", err)
		}

		pub_key, err := pkg.FormatPubKey(publicKey)
		if err != nil {
			log.Fatal("Public key validation failed: ", err)
		}

		// Download the artifact set within flag artifactURL

		log.Info("Download artifact..")

		resp, err := http.DefaultClient.Get(artifactURL)
		if err != nil {
			log.Error(err)
		}

		defer resp.Body.Close()

		log.Info("Contents fetched..")

		readBody, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			log.Error("Error reading response body: ", err)
		}

		// Generate Hash for downloaded artifact
		generatedSha := hashGenerator(artifactURL, readBody)

		// Verify the artifact signing itself
		pubkeyRingReader, err := os.Open(publicKey)
		if err != nil {
			log.Error("Error opening publickey: ", err)
		}
		sigkeyRingReader, err := os.Open(signature)
		if err != nil {
			log.Error("Error opening signature: ", err)
		}

		var keyRing openpgp.EntityList
		if isArmorProtected(pubkeyRingReader) {
			keyRing, err = openpgp.ReadArmoredKeyRing(pubkeyRingReader)
			if err != nil {
				log.Error("Error reading Armored Keyring: ", err)
			}
		} else {
			keyRing, err = openpgp.ReadKeyRing(pubkeyRingReader)
			if err != nil {
				log.Error("Error reading Keyring: ", err)
			}
		}

		dataReader := bytes.NewReader(readBody)

		if isArmorProtected(sigkeyRingReader) {
			_, err = openpgp.CheckArmoredDetachedSignature(keyRing, dataReader, sigkeyRingReader)
			if err != nil {
				log.Error("Error reading Armor Detatched Signature: ", err)
			}
		} else {
			_, err = openpgp.CheckDetachedSignature(keyRing, dataReader, sigkeyRingReader)
			if err != nil {
				log.Error("Error reading Detatched Signature: ", err)
			}
		}
		if err != nil {
			log.Error("Signature Verification failed: ", err)
			os.Exit(1)
		}
		log.Info("Signature validation passed")

		// Construct rekor json file
		// We need to approach this in two ways
		// as the public key and signature could be either
		// armored or binary
		var marshalledRekorEntry []byte
		if isArmorProtected(sigkeyRingReader) || isArmorProtected(pubkeyRingReader) {
			rekorArmorJSON := RekorArmorEntry{
				URL:       artifactURL,
				SHA:       generatedSha,
				Signature: sig,
				PublicKey: pub_key,
			}
			marshalledRekorEntry, err = json.Marshal(rekorArmorJSON)
			if err != nil {
				log.Fatal(err)
			}
		} else {
			pubKey, err := ioutil.ReadFile(publicKey)
			if err != nil {
				log.Fatal("Error Loading: ", err)
			}
			sigKey, err := ioutil.ReadFile(signature)
			if err != nil {
				log.Fatal("Error Loading: ", err)
			}
			rekorJSON := RekorEntry{
				URL:       artifactURL,
				SHA:       generatedSha,
				Signature: sigKey,
				PublicKey: pubKey,
			}
			marshalledRekorEntry, err = json.Marshal(rekorJSON)
			if err != nil {
				log.Fatal("JSON Failed to Marshall: ", err)
			}
		}

		// Upload to the rekor service
		log.Info("Uploading manifest to Rekor.")
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()

		request, err := http.NewRequestWithContext(ctx, "POST", url, nil)
		if err != nil {
			log.Fatal(err)
		}

		request.Body = ioutil.NopCloser(bytes.NewReader(marshalledRekorEntry))
		client := &http.Client{}
		response, err := client.Do(request)

		if err != nil {
			log.Fatal(err)
		}
		defer response.Body.Close()

		content, err := ioutil.ReadAll(response.Body)

		if err != nil {
			log.Fatal(err)
		}

		leafresp := getLeafResponse{}

		if err := json.Unmarshal(content, &leafresp); err != nil {
			log.Fatal(err)
		}

		log.Info("Status: ", leafresp.Status)
	},
}

func init() {
	rootCmd.AddCommand(uploadCmd)
}
