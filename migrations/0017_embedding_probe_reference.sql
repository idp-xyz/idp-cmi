-- ADR-0001 D59: the reference vector of the query endpoint's probe. The first time the documents endpoint answers the probe
-- text its vector is kept here, under the model identity the stored vectors carry (embedding_model, which includes the
-- document-prefix tag, D37), so that after a restart the query endpoint is checked against it without asking the documents
-- endpoint, which a bulk ingest may keep busy for hours. Not tenant data: the rows belong to the deployment and only the
-- embedding workers (app.role = 'embedder') read and write them.
CREATE TABLE embedding_probe_references (
  model        text NOT NULL,
  probe_sha256 text NOT NULL,   -- of the probe text: a changed text needs a new reference
  vector       real[] NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (model, probe_sha256)
);
ALTER TABLE embedding_probe_references ENABLE ROW LEVEL SECURITY;
ALTER TABLE embedding_probe_references FORCE ROW LEVEL SECURITY;
CREATE POLICY embedder ON embedding_probe_references USING (current_setting('app.role', true) = 'embedder') WITH CHECK (current_setting('app.role', true) = 'embedder');
GRANT SELECT, INSERT, UPDATE ON embedding_probe_references TO idp_cmi_app;
