package storage_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/bangbc-luna/lunain-travel/internal/platform/storage"
)

func TestS3_PutAndPresign(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a MinIO container")
	}
	ctx := context.Background()
	ctr, err := tcminio.Run(ctx, minioImage(),
		tcminio.WithUsername("testkey"), tcminio.WithPassword("testsecret123"))
	require.NoError(t, err)
	testcontainers.CleanupContainer(t, ctr)
	endpoint, err := ctr.ConnectionString(ctx)
	require.NoError(t, err)
	endpoint = "http://" + endpoint

	// create the bucket with a plain SDK client
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("testkey", "testsecret123", "")))
	require.NoError(t, err)
	admin := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	_, err = admin.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("lunain")})
	require.NoError(t, err)

	st, err := storage.NewS3(ctx, storage.S3Config{
		Endpoint: endpoint, Region: "us-east-1", Bucket: "lunain",
		AccessKey: "testkey", SecretKey: "testsecret123", UsePathStyle: true,
	})
	require.NoError(t, err)

	content := "hello lunain private file"
	key := "private/payout_proof/2026/11/abc.pdf"
	require.NoError(t, st.Put(ctx, key, "application/pdf", strings.NewReader(content), int64(len(content))))

	u, err := st.PresignGet(ctx, key, 5*time.Minute)
	require.NoError(t, err)
	res, err := http.Get(u)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, "application/pdf", res.Header.Get("Content-Type"))
	got, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.Equal(t, content, string(got))
}

// minioImage allows pointing the test at a mirrored image (MINIO_TEST_IMAGE).
func minioImage() string {
	if img := os.Getenv("MINIO_TEST_IMAGE"); img != "" {
		return img
	}
	return "minio/minio:RELEASE.2024-01-16T16-07-38Z"
}
