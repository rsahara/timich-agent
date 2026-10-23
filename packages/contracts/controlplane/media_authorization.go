package controlplane

import "time"

// The maximum age is anchored to the caller's send time, never its receive time.
const MediaAuthorizationMaxAge = time.Minute

func CheckMediaAuthorizationFullMethod() string {
	return "/" + ServiceFullName() + "/CheckMediaAuthorization"
}
