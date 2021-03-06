// Copyright 2016-2017 Authors of Cilium
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"

	"github.com/cilium/cilium/pkg/endpoint"
	"github.com/cilium/cilium/pkg/policy"

	"github.com/spf13/cobra"
)

// Fatalf prints the Printf formatted message to stderr and exits the program
// Note: os.Exit(1) is not recoverable
func Fatalf(msg string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "Error: %s\n", fmt.Sprintf(msg, args...))
	os.Exit(1)
}

// Usagef prints the Printf formatted message to stderr, prints usage help and
// exits the program
// Note: os.Exit(1) is not recoverable
func Usagef(cmd *cobra.Command, msg string, args ...interface{}) {
	txt := fmt.Sprintf(msg, args...)
	fmt.Fprintf(os.Stderr, "Error: %s\n\n", txt)
	cmd.Help()
	os.Exit(1)
}

func requireEndpointID(cmd *cobra.Command, args []string) {
	if len(args) < 1 {
		Usagef(cmd, "Missing endpoint id argument")
	}

	if id := policy.ReservedIdentities[args[0]]; id == policy.IdentityUnknown {
		_, _, err := endpoint.ValidateID(args[0])

		if err != nil {
			Fatalf("Cannot parse endpoint id \"%s\": %s", args[0], err)
		}
	}
}

func requireEndpointIDorGlobal(cmd *cobra.Command, args []string) {
	if len(args) < 1 {
		Usagef(cmd, "Missing endpoint id or 'global' argument")
	}

	if args[0] != "global" {
		requireEndpointID(cmd, args)
	}
}

func requirePath(cmd *cobra.Command, args []string) {
	if len(args) < 1 {
		Usagef(cmd, "Missing path argument")
	}

	if args[0] == "" {
		Usagef(cmd, "Empty path argument")
	}
}

func requireServiceID(cmd *cobra.Command, args []string) {
	if len(args) < 1 {
		Usagef(cmd, "Missing service id argument")
	}

	if args[0] == "" {
		Usagef(cmd, "Empty service id argument")
	}
}

// Search 'result' for strings with escaped JSON inside, and expand the JSON.
func expandNestedJSON(result bytes.Buffer) (bytes.Buffer, error) {
	reStringWithJSON := regexp.MustCompile(`"[^"\\{]*{.*[^\\]"`)
	reJSON := regexp.MustCompile(`{.*}`)
	for {
		var (
			loc    []int
			indent string
		)

		// Search for nested JSON; if we don't find any, then break.
		resBytes := result.Bytes()
		if loc = reStringWithJSON.FindIndex(resBytes); loc == nil {
			break
		}

		// Determine the current indentation
		for i := 0; i < loc[0]-1; i++ {
			idx := loc[0] - i - 1
			if resBytes[idx] != ' ' {
				break
			}
			indent = fmt.Sprintf("%s ", indent)
		}

		stringStart := loc[0]
		stringEnd := loc[1]

		// Unquote the string with the nested json.
		quotedBytes := resBytes[stringStart:stringEnd]
		unquoted, err := strconv.Unquote(string(quotedBytes))
		if err != nil {
			return bytes.Buffer{}, fmt.Errorf("Failed to Unquote string: %s\n%s", err.Error(), string(quotedBytes))
		}

		// Find the JSON within the quoted string.
		nestedStart := 0
		nestedEnd := 0
		if locs := reJSON.FindAllStringIndex(unquoted, -1); locs != nil {
			// The last match is the longest one.
			last := len(locs) - 1
			nestedStart = locs[last][0]
			nestedEnd = locs[last][1]
		} else if reJSON.Match(quotedBytes) {
			// The entire string is JSON
			nestedEnd = len(unquoted)
		}

		// Decode the nested JSON
		decoded := ""
		if nestedEnd != 0 {
			m := make(map[string]interface{})
			nested := bytes.NewBufferString(unquoted[nestedStart:nestedEnd])
			if err := json.NewDecoder(nested).Decode(&m); err != nil {
				return bytes.Buffer{}, fmt.Errorf("Failed to decode nested JSON: %s", err.Error())
			}
			decodedBytes, err := json.MarshalIndent(m, indent, "  ")
			if err != nil {
				return bytes.Buffer{}, fmt.Errorf("Cannot marshal nested JSON: %s", err.Error())
			}
			decoded = string(decodedBytes)
		}

		// Serialize
		nextResult := bytes.Buffer{}
		nextResult.Write(resBytes[0:stringStart])
		nextResult.WriteString(string(unquoted[:nestedStart]))
		nextResult.WriteString(string(decoded))
		nextResult.WriteString(string(unquoted[nestedEnd:]))
		nextResult.Write(resBytes[stringEnd:])
		result = nextResult
	}

	return result, nil
}
