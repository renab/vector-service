BEGIN;

CREATE INDEX vector_records_qwen3_06b_1024_cosine_hnsw
ON vector_data.vector_records
USING hnsw (
    (embedding::vector(1024)) vector_cosine_ops
)
WHERE vector_space_id = '0199f31e-1000-7000-8000-000000000001';

COMMIT;
