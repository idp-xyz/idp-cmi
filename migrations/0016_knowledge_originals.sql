-- Optional original object for a document version. The key is not part of the contract.

ALTER TABLE knowledge_documents ADD COLUMN original_key text;
ALTER TABLE knowledge_documents ADD COLUMN original_sha256 text;
ALTER TABLE knowledge_documents ADD COLUMN original_byte_size int;
ALTER TABLE knowledge_documents ADD COLUMN original_media_type text;
