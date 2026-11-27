package account

// What deleting an account removes of its music. The deletion and the administrator's preview both use these, so
// they always agree. $1 is the user id.
//
//   - An artist the person created goes, with everything in it.
//   - An artist they received from the catalog stays: only what they uploaded themselves goes.
//   - Of other artists (a former owner, or an administrator who added catalog songs) their songs stay; only their
//     uploads that never became songs (waiting for a file, or failed) go.

// DoomedUploads is true for an upload (alias u) that is deleted with the account.
const DoomedUploads = `(
	EXISTS (SELECT 1 FROM artists da WHERE da.id = u.artist_id AND da.owner_user_id = $1::uuid
	        AND (da.assigned_at IS NULL OR (u.user_id IS NOT DISTINCT FROM $1::uuid AND NOT u.by_team)))
	OR (u.user_id IS NOT DISTINCT FROM $1::uuid AND u.artist_id IS NULL)
	OR (u.user_id IS NOT DISTINCT FROM $1::uuid AND u.status IN ('pending', 'failed')
	    AND NOT EXISTS (SELECT 1 FROM artists da WHERE da.id = u.artist_id AND da.owner_user_id = $1::uuid)))`

// DoomedTracks lists the ids of the tracks deleted with the account.
const DoomedTracks = `SELECT t.id FROM tracks t JOIN artists a ON a.id = t.artist_id
	WHERE a.owner_user_id = $1::uuid AND (a.assigned_at IS NULL
	   OR EXISTS (SELECT 1 FROM uploads u WHERE u.track_id = t.id AND u.status = 'ready' AND ` + DoomedUploads + `))`

// DoomedReleases lists the ids of the releases deleted with the account. Of a received artist, a release goes only
// when everything in it was the person's own.
const DoomedReleases = `SELECT al.id FROM albums al JOIN artists a ON a.id = al.artist_id
	WHERE a.owner_user_id = $1::uuid AND (a.assigned_at IS NULL OR (
	      EXISTS (SELECT 1 FROM uploads u WHERE u.release_id = al.id AND ` + DoomedUploads + `)
	  AND NOT EXISTS (SELECT 1 FROM uploads u WHERE u.release_id = al.id AND NOT ` + DoomedUploads + `)
	  AND NOT EXISTS (SELECT 1 FROM tracks t WHERE t.album_id = al.id AND NOT EXISTS (
	          SELECT 1 FROM uploads u WHERE u.track_id = t.id AND u.status = 'ready' AND ` + DoomedUploads + `))))`
