// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Tamir Duberstein (tamird@gmail.com)

package main

import (
	"flag"
	"log"
	"net/url"
	"os"
	"strconv"

	"github.com/abourget/teamcity"
	"github.com/kisielk/gotool"
)

var buildTypeID = flag.String("build", "Cockroach_Nightlies_Stress", "the TeamCity build ID to start")
var branchName = flag.String("branch", "", "the VCS branch to build")

const teamcityServerURLEnv = "TC_SERVER_URL"
const teamcityAPIUserEnv = "TC_API_USER"
const teamcityAPIPasswordEnv = "TC_API_PASSWORD"

func main() {
	flag.Parse()

	serverURL, ok := os.LookupEnv(teamcityServerURLEnv)
	if !ok {
		log.Fatalf("teamcity server URL environment variable %s is not set", teamcityServerURLEnv)
	}
	u, err := url.Parse(serverURL)
	if err != nil {
		log.Fatal(err)
	}
	username, ok := os.LookupEnv(teamcityAPIUserEnv)
	if !ok {
		log.Fatalf("teamcity API username environment variable %s is not set", teamcityAPIUserEnv)
	}
	password, ok := os.LookupEnv(teamcityAPIPasswordEnv)
	if !ok {
		log.Fatalf("teamcity API password environment variable %s is not set", teamcityAPIPasswordEnv)
	}
	importPaths := gotool.ImportPaths([]string{"github.com/cockroachdb/cockroach/..."})

	client := teamcity.New(u.Host, username, password)
	// Queue a build per configuration per package.
	for _, propEvalKV := range []bool{true, false} {
		for _, params := range []map[string]string{
			{}, // uninstrumented
			{"env.GOFLAGS": "-race"},
			{"env.TAGS": "deadlock"},
		} {
			for _, importPath := range importPaths {
				params["env.PKG"] = importPath
				params["env.COCKROACH_PROPOSER_EVALUATED_KV"] = strconv.FormatBool(propEvalKV)
				build, err := client.QueueBuild(*buildTypeID, *branchName, params)
				if err != nil {
					log.Fatalf("failed to create teamcity build (*buildTypeID=%s *branchName=%s, params=%+v): %s", *buildTypeID, *branchName, params, err)
				}
				log.Printf("created teamcity build (*buildTypeID=%s *branchName=%s, params=%+v): %s", *buildTypeID, *branchName, params, build)
			}
		}
	}
}
