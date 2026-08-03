package kodo

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

const (
	DefaultBaseURL = "https://api.qiniuapi.com"
	BucketWidth    = 5 * time.Minute
)

var (
	ErrInvalidInput       = errors.New("kodo: invalid input")
	ErrUnexpectedResponse = errors.New("kodo: unexpected response")
	ErrMismatchedArrays   = errors.New("kodo: response arrays have different lengths")
	ErrInsufficientPoints = errors.New("kodo: no usable safe point")
	ErrNonContinuous      = errors.New("kodo: latest safe point is not continuous")
)

// Doer is normally an authhttp client that signs the already constructed
// request with Qiniu v2 credentials before sending it.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

type StorageClass string

const (
	StorageClassStandard           StorageClass = "standard"
	StorageClassIA                 StorageClass = "ia"
	StorageClassIntelligentTiering StorageClass = "intelligent_tiering"
	StorageClassArchiveIR          StorageClass = "archive_ir"
	StorageClassArchive            StorageClass = "archive"
	StorageClassDeepArchive        StorageClass = "deep_archive"
)

type StorageEndpoints struct {
	CapacityPath    string
	ObjectCountPath string
}

var endpointsByStorageClass = map[StorageClass]StorageEndpoints{
	StorageClassStandard: {
		CapacityPath:    "/v6/space",
		ObjectCountPath: "/v6/count",
	},
	StorageClassIA: {
		CapacityPath:    "/v6/space_line",
		ObjectCountPath: "/v6/count_line",
	},
	StorageClassIntelligentTiering: {
		CapacityPath:    "/v6/space_intelligent_tiering",
		ObjectCountPath: "/v6/count_intelligent_tiering",
	},
	StorageClassArchiveIR: {
		CapacityPath:    "/v6/space_archive_ir",
		ObjectCountPath: "/v6/count_archive_ir",
	},
	StorageClassArchive: {
		CapacityPath:    "/v6/space_archive",
		ObjectCountPath: "/v6/count_archive",
	},
	StorageClassDeepArchive: {
		CapacityPath:    "/v6/space_deep_archive",
		ObjectCountPath: "/v6/count_deep_archive",
	},
}

func EndpointsForStorageClass(storageClass StorageClass) (StorageEndpoints, error) {
	endpoints, ok := endpointsByStorageClass[storageClass]
	if !ok {
		return StorageEndpoints{}, fmt.Errorf("%w: unsupported storage class %q", ErrInvalidInput, storageClass)
	}
	return endpoints, nil
}

type GaugeKind string

const (
	GaugeStorageBytes         GaugeKind = "storage_bytes"
	GaugeObjects              GaugeKind = "objects"
	GaugeRequestsPerSecond    GaugeKind = "requests_per_second"
	GaugeEgressBytesPerSecond GaugeKind = "egress_bytes_per_second"
)

type Operation string

const (
	OperationGet Operation = "get"
	OperationPut Operation = "put"
)

type Route string

const (
	RouteDirect    Route = "direct"
	RouteCDNOrigin Route = "cdn_origin"
)

// GaugeSample is an intermediate collector value. Fields that do not apply to
// a metric are empty; for example activity samples have no StorageClass in P0.
type GaugeSample struct {
	Kind         GaugeKind
	Bucket       string
	Region       string
	StorageClass StorageClass
	Operation    Operation
	Route        Route
	Value        float64
	DataAt       time.Time
}

// Query identifies one static bucket and the exact upstream time window.
// A point is safe only when its five-minute bucket ends at or before
// SafeBefore. Begin is inclusive and End is exclusive in the Qiniu API.
type Query struct {
	Bucket     string
	Region     string
	Begin      time.Time
	End        time.Time
	SafeBefore time.Time
}

type CollectInput struct {
	Query          Query
	StorageClasses []StorageClass
}

// Point represents one upstream five-minute bucket. Time is the timestamp
// returned by Qiniu and Value is the raw value before rate conversion.
type Point struct {
	Time  time.Time
	Value float64
}

type HTTPStatusError struct {
	StatusCode int
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("kodo: unexpected HTTP status %d", e.StatusCode)
}
