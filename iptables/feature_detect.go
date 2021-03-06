// Copyright (c) 2018-2020 Tigera, Inc. All rights reserved.
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

package iptables

import (
	"bytes"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/projectcalico/felix/versionparse"
)

var (
	vXDotYDotZRegexp = regexp.MustCompile(`v(\d+\.\d+\.\d+)`)

	// iptables versions:
	// v1Dot4Dot7 is the oldest version we've ever supported.
	v1Dot4Dot7 = versionparse.MustParseVersion("1.4.7")
	// v1Dot6Dot0 added --random-fully to SNAT.
	v1Dot6Dot0 = versionparse.MustParseVersion("1.6.0")
	// v1Dot6Dot2 added --random-fully to MASQUERADE and the xtables lock to iptables-restore.
	v1Dot6Dot2 = versionparse.MustParseVersion("1.6.2")

	// Linux kernel versions:
	// v3Dot10Dot0 is the oldest version we support at time of writing.
	v3Dot10Dot0 = versionparse.MustParseVersion("3.10.0")
	// v3Dot14Dot0 added the random-fully feature on the iptables interface.
	v3Dot14Dot0 = versionparse.MustParseVersion("3.14.0")
)

type Features struct {
	// SNATFullyRandom is true if --random-fully is supported by the SNAT action.
	SNATFullyRandom bool
	// MASQFullyRandom is true if --random-fully is supported by the MASQUERADE action.
	MASQFullyRandom bool
	// RestoreSupportsLock is true if the iptables-restore command supports taking the xtables lock and the
	// associated -w and -W arguments.
	RestoreSupportsLock bool
}

type FeatureDetector struct {
	lock         sync.Mutex
	featureCache *Features

	// Path to file with kernel version
	GetKernelVersionReader func() (io.Reader, error)
	// Factory for making commands, used by UTs to shim exec.Command().
	NewCmd cmdFactory
}

func NewFeatureDetector() *FeatureDetector {
	return &FeatureDetector{
		GetKernelVersionReader: versionparse.GetKernelVersionReader,
		NewCmd:                 NewRealCmd,
	}
}

func (d *FeatureDetector) GetFeatures() *Features {
	d.lock.Lock()
	defer d.lock.Unlock()

	if d.featureCache == nil {
		d.refreshFeaturesLockHeld()
	}

	return d.featureCache
}

func (d *FeatureDetector) RefreshFeatures() {
	d.lock.Lock()
	defer d.lock.Unlock()

	d.refreshFeaturesLockHeld()
}

func (d *FeatureDetector) refreshFeaturesLockHeld() {
	// Get the versions.  If we fail to detect a version for some reason, we use a safe default.
	log.Debug("Refreshing detected iptables features")
	iptV := d.getIptablesVersion()
	kerV := d.getKernelVersion()

	// Calculate the features.
	features := Features{
		SNATFullyRandom:     iptV.Compare(v1Dot6Dot0) >= 0 && kerV.Compare(v3Dot14Dot0) >= 0,
		MASQFullyRandom:     iptV.Compare(v1Dot6Dot2) >= 0 && kerV.Compare(v3Dot14Dot0) >= 0,
		RestoreSupportsLock: iptV.Compare(v1Dot6Dot2) >= 0,
	}

	if d.featureCache == nil || *d.featureCache != features {
		log.WithFields(log.Fields{
			"features":        features,
			"kernelVersion":   kerV,
			"iptablesVersion": iptV,
		}).Info("Updating detected iptables features")
		d.featureCache = &features
	}
}

func (d *FeatureDetector) getIptablesVersion() *versionparse.Version {
	cmd := d.NewCmd("iptables", "--version")
	out, err := cmd.Output()
	if err != nil {
		log.WithError(err).Warn("Failed to get iptables version, assuming old version with no optional features")
		return v1Dot4Dot7
	}
	s := string(out)
	log.WithField("rawVersion", s).Debug("Ran iptables --version")
	matches := vXDotYDotZRegexp.FindStringSubmatch(s)
	if len(matches) == 0 {
		log.WithField("rawVersion", s).Warn(
			"Failed to parse iptables version, assuming old version with no optional features")
		return v1Dot4Dot7
	}
	parsedVersion, err := versionparse.NewVersion(matches[1])
	if err != nil {
		log.WithField("rawVersion", s).WithError(err).Warn(
			"Failed to parse iptables version, assuming old version with no optional features")
		return v1Dot4Dot7
	}
	log.WithField("version", parsedVersion).Debug("Parsed iptables version")
	return parsedVersion
}

func (d *FeatureDetector) getKernelVersion() *versionparse.Version {
	reader, err := d.GetKernelVersionReader()
	if err != nil {
		log.WithError(err).Warn("Failed to get the kernel version reader, assuming old version with no optional features")
		return v3Dot10Dot0
	}
	kernVersion, err := versionparse.GetKernelVersion(reader)
	if err != nil {
		log.WithError(err).Warn("Failed to get kernel version, assuming old version with no optional features")
		return v3Dot10Dot0
	}
	return kernVersion
}

func countRulesInIptableOutput(in []byte) int {
	count := 0
	for _, x := range bytes.Split(in, []byte("\n")) {
		if len(x) >= 1 && x[0] == '-' {
			count++
		}
	}
	return count
}

// GetIptablesBackend attempts to detect the iptables backend being used where Felix is running.
// This code is duplicating the detection method found at
// https://github.com/kubernetes/kubernetes/blob/623b6978866b5d3790d17ff13601ef9e7e4f4bf0/build/debian-iptables/iptables-wrapper#L28
// If there is a specifiedBackend then it is used but if it does not match the detected
// backend then a warning is logged.
func DetectBackend(lookPath func(file string) (string, error), newCmd cmdFactory, specifiedBackend string) string {
	ip6LgcySave := findBestBinary(lookPath, 6, "legacy", "save")
	ip4LgcySave := findBestBinary(lookPath, 4, "legacy", "save")
	ip6l, _ := newCmd(ip6LgcySave).Output()
	ip4l, _ := newCmd(ip4LgcySave).Output()
	log.WithField("ip6l", string(ip6l)).Debug("Ip6tables legacy save out")
	log.WithField("ip4l", string(ip4l)).Debug("Iptables legacy save out")
	legacyLines := countRulesInIptableOutput(ip6l) + countRulesInIptableOutput(ip4l)
	var detectedBackend string
	if legacyLines >= 10 {
		detectedBackend = "legacy"
	} else {
		ip6NftSave := findBestBinary(lookPath, 6, "nft", "save")
		ip4NftSave := findBestBinary(lookPath, 4, "nft", "save")
		ip6n, _ := newCmd(ip6NftSave).Output()
		log.WithField("ip6n", string(ip6n)).Debug("Ip6tables save out")
		ip4n, _ := newCmd(ip4NftSave).Output()
		log.WithField("ip4n", string(ip4n)).Debug("Iptables save out")
		nftLines := countRulesInIptableOutput(ip6n) + countRulesInIptableOutput(ip4n)
		if legacyLines >= nftLines {
			detectedBackend = "legacy"
		} else {
			detectedBackend = "nft"
		}
	}
	log.WithField("detectedBackend", detectedBackend).Debug("Detected Iptables backend")

	specifiedBackend = strings.ToLower(specifiedBackend)
	if specifiedBackend != "auto" {
		if specifiedBackend != detectedBackend {
			log.WithFields(log.Fields{"detectedBackend": detectedBackend, "specifiedBackend": specifiedBackend}).Warn("Iptables backend specified does not match the detected backend, using specified backend")
		}
		return specifiedBackend
	}
	return detectedBackend
}

// findBestBinary tries to find an iptables binary for the specific variant (legacy/nftables mode) and returns the name
// of the binary.  Falls back on iptables-restore/iptables-save if the specific variant isn't available.
// Panics if no binary can be found.
func findBestBinary(lookPath func(file string) (string, error), ipVersion uint8, backendMode, saveOrRestore string) string {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	verInfix := ""
	if ipVersion == 6 {
		verInfix = "6"
	}
	candidates := []string{
		"ip" + verInfix + "tables-" + backendMode + "-" + saveOrRestore,
		"ip" + verInfix + "tables-" + saveOrRestore,
	}

	logCxt := log.WithFields(log.Fields{
		"ipVersion":     ipVersion,
		"backendMode":   backendMode,
		"saveOrRestore": saveOrRestore,
		"candidates":    candidates,
	})

	for _, candidate := range candidates {
		_, err := lookPath(candidate)
		if err == nil {
			logCxt.WithField("command", candidate).Info("Looked up iptables command")
			return candidate
		}
	}

	logCxt.Panic("Failed to find iptables command")
	return ""
}
