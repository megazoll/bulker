package main

import (
	"encoding/json"
	"fmt"
	"github.com/jitsucom/bulker/jitsubase/appbase"
	"io"
	"sync/atomic"
	"time"
)

type RepositoryConfig struct {
	RepositoryURL              string `mapstructure:"REPOSITORY_URL" default:"http://console:3000/api/admin/export/streams-with-destinations"`
	RepositoryAuthToken        string `mapstructure:"REPOSITORY_AUTH_TOKEN"`
	RepositoryRefreshPeriodSec int    `mapstructure:"REPOSITORY_REFRESH_PERIOD_SEC" default:"2"`
}

type Streams struct {
	streams          []*StreamWithDestinations
	apiKeyBindings   map[string]*ApiKeyBinding
	streamsByIds     map[string]*StreamWithDestinations
	streamsByDomains map[string][]*StreamWithDestinations
	lastModified     time.Time
}

func (s *Streams) getStreamByKeyId(keyId string) *ApiKeyBinding {
	return s.apiKeyBindings[keyId]
}

func (s *Streams) GetStreamById(slug string) *StreamWithDestinations {
	return s.streamsByIds[slug]
}

func (s *Streams) GetStreamsByDomain(domain string) []*StreamWithDestinations {
	return s.streamsByDomains[domain]
}

type StreamsRepositoryData struct {
	data atomic.Pointer[Streams]
}

func (s *StreamsRepositoryData) Init(reader io.Reader, tag any) error {
	dec := json.NewDecoder(reader)
	// read open bracket
	_, err := dec.Token()
	if err != nil {
		return fmt.Errorf("error reading open bracket: %v", err)
	}
	streams := make([]*StreamWithDestinations, 0)
	apiKeyBindings := map[string]*ApiKeyBinding{}
	streamsByIds := map[string]*StreamWithDestinations{}
	streamsByDomains := map[string][]*StreamWithDestinations{}
	// while the array contains values
	for dec.More() {
		swd := StreamWithDestinations{}
		err = dec.Decode(&swd)
		if err != nil {
			return fmt.Errorf("Error unmarshalling stream config: %v", err)
		}
		swd.init()
		streams = append(streams, &swd)
		streamsByIds[swd.Stream.Id] = &swd
		for _, domain := range swd.Stream.Domains {
			domainStreams, ok := streamsByDomains[domain]
			if !ok {
				domainStreams = make([]*StreamWithDestinations, 0, 1)
			}
			streamsByDomains[domain] = append(domainStreams, &swd)
		}
		for _, key := range swd.Stream.PublicKeys {
			apiKeyBindings[key.Id] = &ApiKeyBinding{
				Hash:     key.Hash,
				KeyType:  "browser",
				StreamId: swd.Stream.Id,
			}
		}
		for _, key := range swd.Stream.PrivateKeys {
			apiKeyBindings[key.Id] = &ApiKeyBinding{
				Hash:     key.Hash,
				KeyType:  "s2s",
				StreamId: swd.Stream.Id,
			}
		}
	}

	// read closing bracket
	_, err = dec.Token()
	if err != nil {
		return fmt.Errorf("error reading closing bracket: %v", err)
	}

	data := Streams{
		streams:          streams,
		apiKeyBindings:   apiKeyBindings,
		streamsByIds:     streamsByIds,
		streamsByDomains: streamsByDomains,
	}
	if tag != nil {
		data.lastModified = tag.(time.Time)
	}
	s.data.Store(&data)
	return nil
}

func (s *StreamsRepositoryData) GetData() *Streams {
	return s.data.Load()
}

func (s *StreamsRepositoryData) Store(writer io.Writer) error {
	d := s.data.Load()
	if d != nil {
		encoder := json.NewEncoder(writer)
		err := encoder.Encode(d.streams)
		return err
	}
	return nil
}

func NewStreamsRepository(url, token string, refreshPeriodSec int, cacheDir string) appbase.Repository[Streams] {
	return appbase.NewHTTPRepository[Streams]("streams-with-destinations", url, token, appbase.HTTPTagLastModified, &StreamsRepositoryData{}, 1, refreshPeriodSec, cacheDir)
}

type DataLayout string

const (
	DataLayoutSegmentCompatible  = "segment-compatible"
	DataLayoutSegmentSingleTable = "segment-single-table"
	DataLayoutJitsuLegacy        = "jitsu-legacy"
)

type ApiKey struct {
	Id        string `json:"id"`
	Plaintext string `json:"plaintext"`
	Hash      string `json:"hash"`
	Hint      string `json:"hint"`
}

type ApiKeyBinding struct {
	Hash     string `json:"hash"`
	KeyType  string `json:"keyType"`
	StreamId string `json:"streamId"`
}

type StreamConfig struct {
	Id                          string   `json:"id"`
	Type                        string   `json:"type"`
	WorkspaceId                 string   `json:"workspaceId"`
	Slug                        string   `json:"slug"`
	Name                        string   `json:"name"`
	Domains                     []string `json:"domains"`
	AuthorizedJavaScriptDomains string   `json:"authorizedJavaScriptDomains"`
	PublicKeys                  []ApiKey `json:"publicKeys"`
	PrivateKeys                 []ApiKey `json:"privateKeys"`
	DataLayout                  string   `json:"dataLayout"`
}

type ShortDestinationConfig struct {
	TagDestinationConfig
	Id              string         `json:"id"`
	ConnectionId    string         `json:"connectionId"`
	DestinationType string         `json:"destinationType"`
	Options         map[string]any `json:"options,omitempty"`
	Credentials     map[string]any `json:"credentials,omitempty"`
}

type TagDestinationConfig struct {
	Mode string `json:"mode,omitempty"`
	Code string `json:"code,omitempty"`
}

type StreamWithDestinations struct {
	Stream                   StreamConfig             `json:"stream"`
	UpdateAt                 time.Time                `json:"updatedAt"`
	BackupEnabled            bool                     `json:"backupEnabled"`
	Destinations             []ShortDestinationConfig `json:"destinations"`
	SynchronousDestinations  []*ShortDestinationConfig
	AsynchronousDestinations []*ShortDestinationConfig
}

func (s *StreamWithDestinations) init() {
	s.SynchronousDestinations = make([]*ShortDestinationConfig, 0)
	s.AsynchronousDestinations = make([]*ShortDestinationConfig, 0)
	for _, d := range s.Destinations {
		if d.Id == "" || d.DestinationType == "" {
			continue
		}
		_, ok := DeviceOptions[d.DestinationType]
		if ok {
			s.SynchronousDestinations = append(s.SynchronousDestinations, &d)
		} else {
			s.AsynchronousDestinations = append(s.AsynchronousDestinations, &d)
		}
	}
}
