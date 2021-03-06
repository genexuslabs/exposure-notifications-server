// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package model

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/exposure-notifications-server/internal/base64util"
)

const (
	// 21 Days worth of keys is the maximum per publish request (inclusive)
	maxKeysPerPublish = 21

	// only valid exposure key keyLength
	KeyLength = 16

	// Transmission risk constraints (inclusive..inclusive)
	MinTransmissionRisk = 0 // 0 indicates, no/unknown risk.
	MaxTransmissionRisk = 8

	// Intervals are defined as 10 minute periods, there are 144 of them in a day.
	// IntervalCount constraints (inclusive..inclusive)
	MinIntervalCount = 1
	MaxIntervalCount = 144

	// Self explanatory.
	// oneDay = time.Hour * 24

	// interval length
	intervalLength = 10 * time.Minute
)

// Publish represents the body of the PublishInfectedIds API call.
// Keys: Required and must have length >= 1 and <= 21 (`maxKeysPerPublish`)
// Regions: Array of regions. System defined, must match configuration.
// AppPackageName: The identifier for the mobile application.
//  - Android: The App Package AppPackageName
//  - iOS: The BundleID
// TransmissionRisk: An integer from 0-8 (inclusive) that represents
//  the transmission risk for this publish.
// Verification: The attestation payload for this request. (iOS or Android specific)
//   Base64 encoded.
// VerificationAuthorityName: a string that should be verified against the code provider.
//  Note: This project doesn't directly include a diagnosis code verification System
//        but does provide the ability to configure one in `serverevn.ServerEnv`
type Publish struct {
	Keys                      []ExposureKey `json:"temporaryExposureKeys"`
	Regions                   []string      `json:"regions"`
	AppPackageName            string        `json:"appPackageName"`
	Platform                  string        `json:"platform"`
	DeviceVerificationPayload string        `json:"deviceVerificationPayload"`
	VerificationPayload       string        `json:"verificationPayload"`
	Padding                   string        `json:"padding"`
}

// AndroidNonce returns the Android. This ensures that the data in the request
// is the same data that was used to create the device attestation.
func (p *Publish) AndroidNonce() string {
	// base64 keys are to be lexicographically sorted
	sortedKeys := make([]ExposureKey, len(p.Keys))
	copy(sortedKeys, p.Keys)
	sort.Slice(sortedKeys, func(i int, j int) bool {
		return sortedKeys[i].Key < sortedKeys[j].Key
	})

	// regions are to be uppercased and then lexographically sorted
	sortedRegions := make([]string, len(p.Regions))
	for i, r := range p.Regions {
		sortedRegions[i] = strings.ToUpper(r)
	}
	sort.Strings(sortedRegions)

	keys := make([]string, 0, len(sortedKeys))
	for _, k := range sortedKeys {
		keys = append(keys, fmt.Sprintf("%v.%v.%v.%v", k.Key, k.IntervalNumber, k.IntervalCount, k.TransmissionRisk))
	}

	// The cleartext is a combination of all of the data on the request
	// in a specific order.
	//
	// appPackageName|key[,key]|region[,region]|verificationAuthorityName
	// Keys are encoded as
	//     base64(exposureKey).intervalNumber.IntervalCount.transmissionRisk
	// When there is > 1 key, keys are comma separated.
	// Keys must in sorted order based on the sorting of the base64 exposure key.
	// Regions are uppercased, sorted, and comma separated
	cleartext :=
		p.AppPackageName + "|" +
			strings.Join(keys, ",") + "|" + // where key is b64key.intervalNum.intervalCount
			strings.Join(sortedRegions, ",") + "|" +
			p.VerificationPayload

	// Take the sha256 checksum of that data
	sum := sha256.Sum256([]byte(cleartext))

	// Base64 encode the result.
	return base64.StdEncoding.EncodeToString(sum[:])
}

// ExposureKey is the 16 byte key, the start time of the key and the
// duration of the key. A duration of 0 means 24 hours.
// - ALL fields are REQUIRED and must meet the constraints below.
// Key must be the base64 (RFC 4648) encoded 16 byte exposure key from the device.
// - Base64 encoding should include padding, as per RFC 4648
// - if the key is not exactly 16 bytes in length, the request will be failed
// - that is, the whole batch will fail.
// IntervalNumber must be "reasonable" as in the system won't accept keys that
//   are scheduled to start in the future or that are too far in the past, which
//   is configurable per installation.
// IntervalCount must >= `minIntervalCount` and <= `maxIntervalCount`
//   1 - 144 inclusive.
// transmissionRisk must be >= 0 and <= 8
type ExposureKey struct {
	Key              string `json:"key"`
	IntervalNumber   int32  `json:"rollingStartNumber"`
	IntervalCount    int32  `json:"rollingPeriod"`
	TransmissionRisk int    `json:"transmissionRisk"`
}

// ExposureKeys represents a set of ExposureKey objects as input to
// export file generation utility.
// Keys: Required and must have length >= 1
type ExposureKeys struct {
	Keys []ExposureKey `json:"temporaryExposureKeys"`
}

// Exposure represents the record as stored in the database
// TODO(mikehelmick) - refactor this so that there is a public
// Exposure struct that doesn't have public fields and an
// internal struct that does. Separate out the database model
// from direct access.
// Mark records as writable/nowritable - is exposure key encrypted
type Exposure struct {
	ExposureKey      []byte    `db:"exposure_key"`
	TransmissionRisk int       `db:"transmission_risk"`
	AppPackageName   string    `db:"app_package_name"`
	Regions          []string  `db:"regions"`
	IntervalNumber   int32     `db:"interval_number"`
	IntervalCount    int32     `db:"interval_count"`
	CreatedAt        time.Time `db:"created_at"`
	LocalProvenance  bool      `db:"local_provenance"`
	FederationSyncID int64     `db:"sync_id"`
}

// IntervalNumber calculates the exposure notification system interval
// number based on the input time.
func IntervalNumber(t time.Time) int32 {
	return int32(t.UTC().Unix()) / int32(intervalLength.Seconds())
}

// TruncateWindow truncates a time based on the size of the creation window.
func TruncateWindow(t time.Time, d time.Duration) time.Time {
	return t.Truncate(d)
}

// Transformer represents a configured Publish -> Exposure[] transformer.
type Transformer struct {
	maxExposureKeys     int
	maxIntervalStartAge time.Duration // How many intervals old does this server accept?
	truncateWindow      time.Duration
}

// NewTransformer creates a transformer for turning publish API requests into
// records for insertion into the database. On the call to TransformPublish
// all data is validated according to the transformer that is used.
func NewTransformer(maxExposureKeys int, maxIntervalStartAge time.Duration, truncateWindow time.Duration) (*Transformer, error) {
	if maxExposureKeys < 0 || maxExposureKeys > maxKeysPerPublish {
		return nil, fmt.Errorf("maxExposureKeys must be > 0 and <= %v, got %v", maxKeysPerPublish, maxExposureKeys)
	}
	return &Transformer{
		maxExposureKeys:     maxExposureKeys,
		maxIntervalStartAge: maxIntervalStartAge,
		truncateWindow:      truncateWindow,
	}, nil
}

// TransformExposureKey converts individual key data to an exposure entity.
// Validations during the transform include:
//
// * exposure keys are exactly 16 bytes in length after base64 decoding
// * minInterval <= interval number <= maxInterval
// * MinIntervalCount <= interval count <= MaxIntervalCount
//
func TransformExposureKey(exposureKey ExposureKey, appPackageName string, upcaseRegions []string, createdAt time.Time, minIntervalNumber, maxIntervalNumber int32) (*Exposure, error) {
	binKey, err := base64util.DecodeString(exposureKey.Key)
	if err != nil {
		return nil, err
	}

	// Validate individual pieces of the exposure key
	if len(binKey) != KeyLength {
		return nil, fmt.Errorf("invalid key length, %v, must be %v", len(binKey), KeyLength)
	}
	if ic := exposureKey.IntervalCount; ic < MinIntervalCount || ic > MaxIntervalCount {
		return nil, fmt.Errorf("invalid interval count, %v, must be >= %v && <= %v", ic, MinIntervalCount, MaxIntervalCount)
	}

	// Validate the IntervalNumber.
	if exposureKey.IntervalNumber < minIntervalNumber {
		return nil, fmt.Errorf("interval number %v is too old, must be >= %v", exposureKey.IntervalNumber, minIntervalNumber)
	}
	if exposureKey.IntervalNumber >= maxIntervalNumber {
		return nil, fmt.Errorf("interval number %v is in the future, must be < %v", exposureKey.IntervalNumber, maxIntervalNumber)
	}

	// Validate that the key is no longer effective.
	skip, _ := strconv.ParseBool(os.Getenv("SKIP_KEY_DATE_VALIDATION"))
	if !skip && exposureKey.IntervalNumber+exposureKey.IntervalCount > maxIntervalNumber {
		return nil, fmt.Errorf("interval number %v + interval count %v represents a key that is still valid, must end <= %v",
			exposureKey.IntervalNumber, exposureKey.IntervalCount, maxIntervalNumber)
	}

	if tr := exposureKey.TransmissionRisk; tr < MinTransmissionRisk || tr > MaxTransmissionRisk {
		return nil, fmt.Errorf("invalid transmission risk: %v, must be >= %v && <= %v", tr, MinTransmissionRisk, MaxTransmissionRisk)
	}

	return &Exposure{
		ExposureKey:      binKey,
		TransmissionRisk: exposureKey.TransmissionRisk,
		AppPackageName:   appPackageName,
		Regions:          upcaseRegions,
		IntervalNumber:   exposureKey.IntervalNumber,
		IntervalCount:    exposureKey.IntervalCount,
		CreatedAt:        createdAt,
		LocalProvenance:  true,
	}, nil
}

// TransformPublish converts incoming key data to a list of exposure entities.
// The data in the request is validated during the transform, including:
//
// * 0 exposure Keys in the requests
// * > Transformer.maxExposureKeys in the request
//
func (t *Transformer) TransformPublish(inData *Publish, batchTime time.Time) ([]*Exposure, error) {
	// Validate the number of keys that want to be published.
	if len(inData.Keys) == 0 {
		msg := "no exposure keys in publish request"
		return nil, fmt.Errorf(msg)
	}
	if len(inData.Keys) > t.maxExposureKeys {
		msg := fmt.Sprintf("too many exposure keys in publish: %v, max of %v is allowed", len(inData.Keys), t.maxExposureKeys)
		return nil, fmt.Errorf(msg)
	}

	createdAt := TruncateWindow(batchTime, t.truncateWindow)
	entities := make([]*Exposure, 0, len(inData.Keys))

	// An exposure key must have an interval >= minInterval (max configured age)
	minIntervalNumber := IntervalNumber(batchTime.Add(-1 * t.maxIntervalStartAge))
	// And have an interval <= maxInterval (configured allowed clock skew)
	maxIntervalNumber := IntervalNumber(batchTime)

	// Regions are a multi-value property, uppercase them for storage.
	// There is no set of "valid" regions overall, but it is defined
	// elsewhere by what regions an authorized application may write to.
	// See `authorizedapp.Config`
	upcaseRegions := make([]string, len(inData.Regions))
	for i, r := range inData.Regions {
		upcaseRegions[i] = strings.ToUpper(r)
	}

	for _, exposureKey := range inData.Keys {
		exposure, err := TransformExposureKey(exposureKey, inData.AppPackageName, upcaseRegions, createdAt, minIntervalNumber, maxIntervalNumber)
		if err != nil {
			return nil, fmt.Errorf("invalid publish data: %v", err)
		}
		entities = append(entities, exposure)
	}

	// Validate the uploaded data meets configuration parameters.
	// In v1.5+, it is possible to have multiple keys that overlap. They
	// take the form of the same start interval with variable rolling period numbers.
	// Sort by interval number to make necessary checks easier.
	sort.Slice(entities, func(i int, j int) bool {
		if entities[i].IntervalNumber == entities[j].IntervalNumber {
			return entities[i].IntervalCount < entities[j].IntervalCount
		}
		return entities[i].IntervalNumber < entities[j].IntervalNumber
	})

	// Check that any overlapping keys meet configuration.
	// Overlapping keys must have the same start interval. And there is a max number
	// of "same day" keys that are allowed.
	// We do not enforce that keys have UTC midnight aligned start intervals.

	// Running count of start intervals.
	startIntervals := make(map[int32]int)
	lastInterval := entities[0].IntervalNumber
	nextInterval := entities[0].IntervalNumber + entities[0].IntervalCount

	for _, ex := range entities {
		// Relies on the default value of 0 for the map value type.
		startIntervals[ex.IntervalNumber] = startIntervals[ex.IntervalNumber] + 1

		if ex.IntervalNumber == lastInterval {
			// OK, overlaps by start interval. But move out the nextInterval
			nextInterval = ex.IntervalNumber + ex.IntervalCount
			continue
		}

		if ex.IntervalNumber < nextInterval {
			msg := fmt.Sprintf("exposure keys have non aligned overlapping intervals. %v overlaps with previous key that is good from %v to %v.", ex.IntervalNumber, lastInterval, nextInterval)
			return nil, fmt.Errorf(msg)
		}
		// OK, current key starts at or after the end of the previous one. Advance both variables.
		lastInterval = ex.IntervalNumber
		nextInterval = ex.IntervalNumber + ex.IntervalCount
	}

	return entities, nil
}
