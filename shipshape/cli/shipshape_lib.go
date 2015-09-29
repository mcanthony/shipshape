/*
 * Copyright 2014 Google Inc. All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package cli is a command line interface to shipshape.
// It (optionally) pulls a docker container, runs it,
// and runs the analysis service with the specified local
// files and configuration.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"shipshape/service"
	"shipshape/util/docker"
	"shipshape/util/rpc/client"
	glog "third_party/go-glog"

	"github.com/golang/protobuf/proto"

	notepb "shipshape/proto/note_proto"
	ctxpb "shipshape/proto/shipshape_context_proto"
	rpcpb "shipshape/proto/shipshape_rpc_proto"
)

const (
	workspace  = "/shipshape-workspace"
	logsDir    = "/shipshape-output"
	localLogs  = "/tmp"
	image      = "service"
	kytheImage = "kythe"
)

type Invocation struct {
	File                string
	ThirdPartyAnalyzers []string
	// TODO(ciera): make an enum
	Build       string
	TriggerCats []string
	Dind        bool
	Event       string
	JsonOutput  string
	Repo        string
	StayUp      bool
	Tag         string
	LocalKythe  bool
}

func printMessage(msg *rpcpb.ShipshapeResponse, directory string) error {
	fileNotes := make(map[string][]*notepb.Note)
	for _, analysis := range msg.AnalyzeResponse {
		for _, failure := range analysis.Failure {
			fmt.Printf("WARNING: Analyzer %s failed to run: %s\n", *failure.Category, *failure.FailureMessage)
		}
		for _, note := range analysis.Note {
			path := ""
			if note.Location != nil {
				path = filepath.Join(directory, note.Location.GetPath())
			}
			fileNotes[path] = append(fileNotes[path], note)
		}
	}

	for path, notes := range fileNotes {
		if path != "" {
			fmt.Println(path)
		} else {
			fmt.Println("Global")
		}
		for _, note := range notes {
			loc := ""
			subCat := ""
			if note.Subcategory != nil {
				subCat = ":" + *note.Subcategory
			}
			if note.GetLocation().Range != nil && note.GetLocation().GetRange().StartLine != nil {
				if note.GetLocation().GetRange().StartColumn != nil {
					loc = fmt.Sprintf("Line %d, Col %d ", *note.Location.Range.StartLine, *note.Location.Range.StartColumn)
				} else {
					loc = fmt.Sprintf("Line %d ", *note.Location.Range.StartLine)
				}
			}

			fmt.Printf("%s[%s%s]\n", loc, *note.Category, subCat)
			fmt.Printf("\t%s\n", *note.Description)
		}
		fmt.Println()
	}
	return nil
}

func logMessage(msg *rpcpb.ShipshapeResponse, directory string, jsonFile string) error {
	// TODO(ciera): these results aren't sorted. They should be sorted by path and start line
	if jsonFile == "" {
		return printMessage(msg, directory)
	}

	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(jsonFile, b, 0644)
}

func (i *Invocation) Run() (int, error) {
	glog.Infof("Starting shipshape...")
	fs, err := os.Stat(i.File)
	if err != nil {
		return 0, fmt.Errorf("%s is not a valid file or directory\n", i.File)
	}

	origDir := i.File
	if !fs.IsDir() {
		origDir = filepath.Dir(i.File)
	}

	absRoot, err := filepath.Abs(origDir)
	if err != nil {
		return 0, fmt.Errorf("could not get absolute path for %s: %v\n", origDir, err)
	}

	if !docker.HasDocker() {
		return 0, fmt.Errorf("docker could not be found. Make sure you have docker installed.")
	}

	image := docker.FullImageName(i.Repo, image, i.Tag)
	glog.Infof("Starting shipshape using %s on %s", image, absRoot)

	// Create the request

	if len(i.TriggerCats) == 0 {
		glog.Infof("No categories provided. Will be using categories specified by the config file for the event %s", i.Event)
	}

	if len(i.ThirdPartyAnalyzers) == 0 {
		i.ThirdPartyAnalyzers, err = service.GlobalConfig(absRoot)
		if err != nil {
			glog.Infof("Could not get global config; using only the default analyzers: %v", err)
		}
	}

	// If we are not running in local mode, pull the latest copy
	// Notice this will use the local tag as a signal to not pull the
	// third-party analyzers either.
	if i.Tag != "local" {
		pull(image)
		pullAnalyzers(i.ThirdPartyAnalyzers)
	}

	// Put in this defer before calling run. Even if run fails, it can
	// still create the container.
	if !i.StayUp {
		// TODO(ciera): Rather than immediately sending a SIGKILL,
		// we should use the default 10 seconds and properly handle
		// SIGTERMs in the endpoint script.
		defer stop("shipping_container", 0)
		// Stop all the analyzers, even the ones that had trouble starting,
		// in case they did actually start
		for id, analyzerRepo := range i.ThirdPartyAnalyzers {
			container, _ := getContainerAndAddress(analyzerRepo, id)
			defer stop(container, 0)
		}
	}

	containers, errs := startAnalyzers(absRoot, i.ThirdPartyAnalyzers, i.Dind)
	for _, err := range errs {
		glog.Errorf("Could not start up third party analyzer: %v", err)
	}

	var c *client.Client
	var req *rpcpb.ShipshapeRequest
	var numNotes int

	// Run it on files
	relativeRoot := ""
	c, relativeRoot, err = startShipshapeService(image, absRoot, containers, i.Dind)
	if err != nil {
		return 0, fmt.Errorf("HTTP client did not become healthy: %v", err)
	}
	var files []string
	if !fs.IsDir() {
		files = []string{filepath.Base(i.File)}
	}
	req = createRequest(i.TriggerCats, files, i.Event, filepath.Join(workspace, relativeRoot), ctxpb.Stage_PRE_BUILD.Enum())
	glog.Infof("Calling with request %v", req)
	numNotes, err = analyze(c, req, origDir, i.JsonOutput)
	if err != nil {
		return numNotes, fmt.Errorf("error making service call: %v", err)
	}

	// If desired, generate compilation units with a kythe image
	if i.Build != "" {
		// TODO(ciera): Handle other build systems
		fullKytheImage := docker.FullImageName(i.Repo, kytheImage, i.Tag)
		if !i.LocalKythe {
			pull(fullKytheImage)
		}

		// TODO(emso): Add a check for an already running kythe container.
		// The below defer should stop the one started below but in case this
		// failed for some reason (or a kythe container was started in some other
		// way) the below run command will fail.
		defer stop("kythe", 10*time.Second)
		glog.Infof("Retrieving compilation units with %s", i.Build)

		result := docker.RunKythe(fullKytheImage, "kythe", absRoot, i.Build, i.Dind)
		if result.Err != nil {
			// kythe spews output, so only capture it if something went wrong.
			printStreams(result)
			return numNotes, fmt.Errorf("error from run: %v", result.Err)
		}
		glog.Infoln("CompilationUnits prepared")

		req.Stage = ctxpb.Stage_POST_BUILD.Enum()
		glog.Infof("Calling with request %v", req)
		numBuildNotes, err := analyze(c, req, origDir, i.JsonOutput)
		numNotes += numBuildNotes
		if err != nil {
			return numNotes, fmt.Errorf("error making service call: %v", err)
		}
	}

	glog.Infoln("End of Results.")
	return numNotes, nil
}

func numNotes(msg *rpcpb.ShipshapeResponse) int {
	numNotes := 0
	for _, analysis := range msg.AnalyzeResponse {
		numNotes += len(analysis.Note)
	}
	return numNotes
}

// startShipshapeService ensures that there is a service started with the given image and
// attached analyzers that can analyze the directory at absRoot (an absolute path). If a
// service is not started up that can do this, it will shut down the existing one and start
// a new one.
// The methods returns the (ready) client, the relative path from the docker container's mapped
// volume to the absRoot that we are analyzing, and any errors from attempting to run the service.
// TODO(ciera): This *should* check the analyzers that are connected, but does not yet
// do so.
func startShipshapeService(image, absRoot string, analyzers []string, dind bool) (*client.Client, string, error) {
	glog.Infof("Starting shipshape...")
	container := "shipping_container"
	// subPath is the relatve path from the mapped volume on shipping container
	// to the directory we are analyzing (absRoot)
	isMapped, subPath := docker.MappedVolume(absRoot, container)
	// Stop and restart the container if:
	// 1: The container is not using the latest image OR
	// 2: The container is not mapped to the right directory OR
	// 3: The container is not linked to the right analyzer containers
	// Otherwise, use the existing container
	if !docker.ImageMatches(image, container) || !isMapped || !docker.ContainsLinks(container, analyzers) {
		glog.Infof("Restarting container with %s", image)
		stop(container, 0)
		result := docker.RunService(image, container, absRoot, localLogs, analyzers, dind)
		subPath = ""
		printStreams(result)
		if result.Err != nil {
			return nil, "", result.Err
		}
	}
	glog.Infof("Image %s running in service mode", image)
	c := client.NewHTTPClient("localhost:10007")
	return c, subPath, c.WaitUntilReady(10 * time.Second)
}

func analyze(c *client.Client, req *rpcpb.ShipshapeRequest, originalDir, jsonFile string) (int, error) {
	var totalNotes = 0
	glog.Infof("Calling to the shipshape service with %v", req)
	rd := c.Stream("/ShipshapeService/Run", req)
	defer rd.Close()
	for {
		var msg rpcpb.ShipshapeResponse
		if err := rd.NextResult(&msg); err == io.EOF {
			break
		} else if err != nil {
			return 0, fmt.Errorf("received an error from calling run: %v", err.Error())
		}

		err := logMessage(&msg, originalDir, jsonFile)
		if err != nil {
			return 0, fmt.Errorf("could not parse results: %v", err.Error())
		}
		totalNotes += numNotes(&msg)
	}
	return totalNotes, nil
}

func pull(image string) {
	if !docker.OutOfDate(image) {
		return
	}
	glog.Infof("Pulling image %s", image)
	result := docker.Pull(image)
	printStreams(result)
	if result.Err != nil {
		glog.Errorf("Error from pull: %v", result.Err)
		return
	}
	glog.Infoln("Pulling complete")
}

func stop(container string, timeWait time.Duration) {
	glog.Infof("Stopping and removing %s", container)
	result := docker.Stop(container, timeWait, true)
	printStreams(result)
	if result.Err != nil {
		glog.Infof("Could not stop %s: %v", container, result.Err)
	} else {
		glog.Infoln("Removed.")
	}
}

func pullAnalyzers(images []string) {
	var wg sync.WaitGroup
	for _, analyzerImage := range images {
		wg.Add(1)
		go func(image string) {
			pull(image)
			wg.Done()
		}(analyzerImage)
	}
	glog.Info("Pulling dockerized analyzers...")
	wg.Wait()
	glog.Info("Analyzers pulled")
}

func startAnalyzers(sourceDir string, images []string, dind bool) (containers []string, errs []error) {
	var wg sync.WaitGroup
	for id, fullImage := range images {
		wg.Add(1)
		go func(id int, image string) {
			analyzerContainer, port := getContainerAndAddress(image, id)
			if docker.ImageMatches(image, analyzerContainer) {
				glog.Infof("Reusing analyzer %v started at localhost:%d", image, port)
			} else {
				glog.Infof("Found no analyzer container (%v) to reuse for %v", analyzerContainer, image)
				// Analyzer is either running with the wrong image version, or not running
				// Stopping in case it's the first case
				result := docker.Stop(analyzerContainer, 0, true)
				if result.Err != nil {
					glog.Infof("Failed to stop %v (may not be running)", analyzerContainer)
				}
				result = docker.RunAnalyzer(image, analyzerContainer, sourceDir, localLogs, port, dind)
				if result.Err != nil {
					glog.Infof("Could not start %v at localhost:%d: %v, stderr: %v", image, port, result.Err.Error(), result.Stderr)
					errs = append(errs, result.Err)
				} else {
					glog.Infof("Analyzer %v started at localhost:%d", image, port)
					containers = append(containers, analyzerContainer)
				}
			}
			wg.Done()
		}(id, fullImage)
	}
	if len(images) > 0 {
		glog.Info("Waiting for dockerized analyzers to start up...")
		wg.Wait()
		glog.Info("Analyzers up")
	}
	return containers, errs
}

func printStreams(result docker.CommandResult) {
	out := strings.TrimSpace(result.Stdout)
	err := strings.TrimSpace(result.Stderr)
	if len(out) > 0 {
		glog.Infof("stdout:\n%s\n", strings.TrimSpace(result.Stdout))
	}
	if len(err) > 0 {
		glog.Errorf("stderr:\n%s\n", strings.TrimSpace(result.Stderr))
	}
}

func getContainerAndAddress(fullImage string, id int) (analyzerContainer string, port int) {
	// A docker image URI (location:port/path:tag) can have a host part
	// with a port number and a path part with a tag.  Both tag and port
	// are separated by colon, so we need to find out if the last colon is
	// the one that separates the tag from the path, or the port in the
	// location.
	end := strings.LastIndex(fullImage, ":")
	slash := strings.LastIndex(fullImage, "/")
	if end == -1 || end < slash {
		// No colon, or last colon is part of the location.
		end = len(fullImage)
	}
	image := fullImage[slash+1 : end]
	port = 10010 + id
	analyzerContainer = fmt.Sprintf("%s_%d", image, id)
	return analyzerContainer, port
}

func createRequest(triggerCats, files []string, event, repoRoot string, stage *ctxpb.Stage) *rpcpb.ShipshapeRequest {
	return &rpcpb.ShipshapeRequest{
		TriggeredCategory: triggerCats,
		ShipshapeContext: &ctxpb.ShipshapeContext{
			RepoRoot: proto.String(repoRoot),
			FilePath: files,
		},
		Event: proto.String(event),
		Stage: stage,
	}
}
