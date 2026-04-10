package docker

import "strings"

// reconcileEnv merges a container's env slice against the old and new image
// env baselines. An entry that matches the old image's baseline is treated
// as inherited from the old image and replaced with the new image's value
// for that key; an entry that differs (or introduces a new key) is treated
// as a user override and preserved across the update.
//
// The output order is: new-image defaults first — with any overridden entry
// replaced in place so relative order stays close to what the image author
// intended — followed by user-added keys that the new image does not set.
// Docker applies env in order and honours last-wins, so this layout keeps
// user intent intact while still letting image-level ENV directives take
// effect on update.
func reconcileEnv(container, oldImage, newImage []string) []string {
	oldImageKV := envToMap(oldImage)

	userKV := make(map[string]string)
	var userOrder []string
	for _, entry := range container {
		k, v, ok := splitEnvEntry(entry)
		if !ok {
			continue
		}
		if oldV, inherited := oldImageKV[k]; inherited && oldV == v {
			continue
		}
		if _, seen := userKV[k]; !seen {
			userOrder = append(userOrder, k)
		}
		userKV[k] = v
	}

	out := make([]string, 0, len(newImage)+len(userKV))
	overridden := make(map[string]bool, len(userKV))
	for _, entry := range newImage {
		k, _, ok := splitEnvEntry(entry)
		if !ok {
			out = append(out, entry)
			continue
		}
		if v, has := userKV[k]; has {
			out = append(out, k+"="+v)
			overridden[k] = true
			continue
		}
		out = append(out, entry)
	}
	for _, k := range userOrder {
		if overridden[k] {
			continue
		}
		out = append(out, k+"="+userKV[k])
	}
	return out
}

func envToMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, entry := range env {
		if k, v, ok := splitEnvEntry(entry); ok {
			out[k] = v
		}
	}
	return out
}

func splitEnvEntry(entry string) (string, string, bool) {
	i := strings.Index(entry, "=")
	if i < 0 {
		return "", "", false
	}
	return entry[:i], entry[i+1:], true
}

// reconcileSlice returns newImage when container matches oldImage element
// for element — meaning the container inherited its value straight from the
// old image's CMD / ENTRYPOINT directive, so the new image's version should
// take over. Any divergence means the user supplied an override at run time
// and we preserve it verbatim.
//
// The ~[]string constraint lets this function accept both plain []string
// and Docker's strslice.StrSlice (a named []string) without conversions at
// the call site.
func reconcileSlice[S ~[]string](container, oldImage, newImage S) S {
	if len(container) != len(oldImage) {
		return container
	}
	for i := range container {
		if container[i] != oldImage[i] {
			return container
		}
	}
	return newImage
}

// reconcileScalar is the string-valued counterpart to reconcileSlice: when
// the container value matches the old image's default, take the new image's
// value; otherwise keep the container's user-set override.
func reconcileScalar(container, oldImage, newImage string) string {
	if container == oldImage {
		return newImage
	}
	return container
}
