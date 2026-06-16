package build

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// S3Exporter handles exporting a VM's built disk to S3.
type S3Exporter struct {
	Client     client.Client
	DiskReader DiskReader
}

// DiskReader provides a way to read the contents of a PVC.
type DiskReader interface {
	ReadDisk(ctx context.Context, pvcName, namespace string) (io.ReadCloser, error)
}

// HandleVMExport exports a single VM's disk to S3.
func (e *S3Exporter) HandleVMExport(ctx context.Context, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM, export *v1alpha1.S3ExportOutput) (v1alpha1.BuildPhase, error) {
	logger := log.FromContext(ctx).WithValues("vm", vmSpec.Name)
	logger.Info("Starting S3 export", "bucket", export.Bucket, "key", export.Key)

	// Load S3 credentials.
	secret := &corev1.Secret{}
	if err := e.Client.Get(ctx, types.NamespacedName{
		Name:      export.CredentialsSecret.Name,
		Namespace: build.Namespace,
	}, secret); err != nil {
		return v1alpha1.BuildPhaseFailed, fmt.Errorf("loading S3 credentials: %w", err)
	}

	accessKey := string(secret.Data["AWS_ACCESS_KEY_ID"])
	secretKey := string(secret.Data["AWS_SECRET_ACCESS_KEY"])
	if accessKey == "" || secretKey == "" {
		return v1alpha1.BuildPhaseFailed, fmt.Errorf("S3 credentials secret must contain AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY")
	}

	// Output DV name is derived from buildID.
	dvName := BuildNameForOutputDV(BuildID(build), vmSpec.Name)
	dvNamespace := BuildNS(build)

	if e.DiskReader == nil {
		return v1alpha1.BuildPhaseFailed, fmt.Errorf("DiskReader not configured — S3 export requires a DiskReader implementation")
	}

	reader, err := e.DiskReader.ReadDisk(ctx, dvName, dvNamespace)
	if err != nil {
		return v1alpha1.BuildPhaseFailed, fmt.Errorf("reading disk: %w", err)
	}
	defer reader.Close() //nolint:errcheck

	// Compute checksum while buffering.
	hash := sha256.New()
	var buf bytes.Buffer
	if _, err := io.Copy(io.MultiWriter(&buf, hash), reader); err != nil {
		return v1alpha1.BuildPhaseFailed, fmt.Errorf("reading disk for upload: %w", err)
	}
	checksum := fmt.Sprintf("sha256:%x", hash.Sum(nil))

	// Upload to S3.
	s3Client := s3.New(s3.Options{
		Region:       export.Region,
		BaseEndpoint: &export.Endpoint,
		Credentials:  credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
	})

	_, err = s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(export.Bucket),
		Key:           aws.String(export.Key),
		Body:          bytes.NewReader(buf.Bytes()),
		ContentLength: aws.Int64(int64(buf.Len())),
	})
	if err != nil {
		return v1alpha1.BuildPhaseFailed, fmt.Errorf("uploading to S3: %w", err)
	}

	// Record the export status.
	build.Status.S3Exports = append(build.Status.S3Exports, v1alpha1.S3ExportStatus{
		VMName:   vmSpec.Name,
		Uploaded: true,
		Location: fmt.Sprintf("s3://%s/%s", export.Bucket, export.Key),
		Checksum: checksum,
	})

	logger.Info("S3 export completed", "location", fmt.Sprintf("s3://%s/%s", export.Bucket, export.Key))
	return v1alpha1.BuildPhaseExporting, nil
}
