-- Format 11 is the first supported offline migration baseline. The existing
-- schema and its markers are validated before Goose can record this step.
-- Earlier unpublished formats are intentionally not adopted or converted.
-- +goose Up
SELECT 1;
