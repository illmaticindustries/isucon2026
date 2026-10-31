-- One durable epoch shared by all regional application processes.
CREATE TABLE IF NOT EXISTS image_cache_generation (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    generation bigint NOT NULL
);
INSERT INTO image_cache_generation (singleton, generation) VALUES (true, 1)
ON CONFLICT (singleton) DO NOTHING;
