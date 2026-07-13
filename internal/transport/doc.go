// Package transport holds the unexported FTP/FTPS/SFTP protocol
// implementations used by the uploadfun engine. It has no dependency on
// the root uploadfun package - callers adapt Endpoint/Uploader (root
// concepts) to/from the plain-value options and clients here.
package transport
