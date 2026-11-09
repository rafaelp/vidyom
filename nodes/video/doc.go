// Package videonodes implements the video.* nodes on FFmpeg (§4, §6.4): probing,
// transcoding to a versioned profile, timeline validation, preview, clip render and
// thumbnails. Every output keeps the source's timeline and is never upscaled (R10,
// R17, D21). video.cut reads only from the master role, with no fallback to archive.
package videonodes
