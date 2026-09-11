ALTER TABLE agent_task_queue
    ADD COLUMN IF NOT EXISTS model_override TEXT;
