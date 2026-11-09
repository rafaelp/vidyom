-- Speaker or instructor, ingest metadata of §3.1 step 1 (I281). The public export writes
-- it to videos.speaker, so §13.2's search by instructor has data.
ALTER TABLE assets ADD COLUMN speaker TEXT NOT NULL DEFAULT '';
