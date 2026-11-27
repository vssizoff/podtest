ALTER TABLE material_revisions ADD COLUMN attachment_id uuid REFERENCES artifacts;
ALTER TABLE material_revisions ADD COLUMN attachment_name text;
ALTER TABLE material_revisions ADD CHECK ((attachment_id IS NULL) = (attachment_name IS NULL));
