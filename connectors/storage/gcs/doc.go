// Package gcs is the Google Cloud Storage connector (§7.2). It is tested only
// against fake-gcs-server, never against real GCS (I12, I199), so its behavior on
// real buckets stays Validate.
//
// Instance fields: bucket, prefix, storageClass, credentialsRef and usage. The
// credential is a service account JSON resolved at open and kept in the client's
// memory. There is no fallback to ambient credentials.
//
// Instance parameters:
//
//	endpoint        an emulator's URL; plain HTTP is accepted on loopback only.
//	                This is the only way to run without a credential
//	versioning      declares that the bucket has object versioning on
//	chunkSizeBytes  resumable upload chunk size, default 16 MiB
//
// GCS has no SHA-256, so the driver computes it with CRC32C while streaming and
// keeps both as object metadata (I186). That lets Verify check an object against
// the server's own CRC32C with no read, which matters on the ARCHIVE class.
package gcs
