package models

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Image-import job states.
const (
	ImportJobRunning   = "running"
	ImportJobCompleted = "completed"
)

// Per-image states within a job.
const (
	ImportImagePending = "pending"
	ImportImageStored  = "stored"
	ImportImageFailed  = "failed"
)

// ImportJobImage is one photograph in an import job.
type ImportJobImage struct {
	// SourceURL is where it is fetched from, and how the admin panel maps the
	// result back to the colourway it belongs to.
	SourceURL string `json:"sourceUrl" bson:"source_url"`
	// ObjectName is decided when the job is created rather than when the
	// image is stored, so the name is known before the worker runs -- which
	// is what lets the same job be retried without producing a second copy.
	ObjectName string `json:"objectName" bson:"object_name"`
	Status     string `json:"status" bson:"status"`
	URL        string `json:"url,omitempty" bson:"url,omitempty"`
	Width      int    `json:"width,omitempty" bson:"width,omitempty"`
	Height     int    `json:"height,omitempty" bson:"height,omitempty"`
	// Reason, when failed, in words an admin can act on.
	Reason string `json:"reason,omitempty" bson:"reason,omitempty"`
	// Warning, when stored but below the catalogue's preferred size.
	Warning string `json:"warning,omitempty" bson:"warning,omitempty"`
}

// ImageImportJob is a batch of photographs being copied into our own storage
// by the worker, one message per image.
//
// A job rather than a request because the copying cannot finish inside one:
// a whole variant group is dozens of photographs, each a download from the
// source and an upload to the bucket, and the API gateway allows a request
// thirty seconds. Splitting them across the worker means the panel gets an
// answer at once and a progress figure it can show, and the number of images
// no longer has to be capped to fit a timeout.
type ImageImportJob struct {
	ID        primitive.ObjectID `json:"id" bson:"_id,omitempty"`
	CreatedAt time.Time          `json:"createdAt" bson:"created_at"`
	UpdatedAt time.Time          `json:"updatedAt" bson:"updated_at"`
	// CreatedBy is the admin who approved the import.
	CreatedBy string `json:"createdBy,omitempty" bson:"created_by,omitempty"`
	// SourceURL is sent as the referer when fetching; some CDNs require it.
	SourceURL string `json:"sourceUrl" bson:"source_url"`
	// Name seeds the object names, so the bucket stays legible.
	Name   string           `json:"name" bson:"name"`
	Status string           `json:"status" bson:"status"`
	Images []ImportJobImage `json:"images" bson:"images"`
}

// Counts summarises progress for the panel.
func (j *ImageImportJob) Counts() (stored, failed, pending int) {
	for _, image := range j.Images {
		switch image.Status {
		case ImportImageStored:
			stored++
		case ImportImageFailed:
			failed++
		default:
			pending++
		}
	}
	return stored, failed, pending
}
