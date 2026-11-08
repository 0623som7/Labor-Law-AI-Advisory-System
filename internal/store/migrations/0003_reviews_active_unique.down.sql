ALTER TABLE reviews
  DROP INDEX uk_reviews_active_file,
  DROP COLUMN active;
