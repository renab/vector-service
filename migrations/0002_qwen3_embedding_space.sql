BEGIN;

INSERT INTO vector_control.vector_spaces (
    id,
    vector_space_key,
    embedding_model,
    embedding_model_version,
    dimensions,
    distance_metric,
    enabled
)
VALUES (
    '0199f31e-1000-7000-8000-000000000001',
    'qwen3-embedding-0.6b-1024-cosine-v1',
    'qwen3-embedding-0.6b',
    'production-v1',
    1024,
    'cosine',
    true
)
ON CONFLICT (vector_space_key) DO NOTHING;

COMMIT;
