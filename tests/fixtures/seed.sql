-- Seeded integration fixture: two tenants with Hermes clients of every
-- authority, carriers, templates, blocks, one message with a receipt per
-- tenant and an unresolved login. Sanitized test data only: every password
-- is "Passw0rd!" (bcrypt cost 4). Provider configurations are sealed by the
-- harness with its test KEK (config_sealed holds a placeholder here).
-- Applied with the migration role; ids are explicit and sequences are
-- advanced afterwards, as after a legacy import.

-- Tenant A: 0b2f6a1e-4c55-4c8e-9d1a-000000000a0a   Tenant B: 0b2f6a1e-4c55-4c8e-9d1a-000000000b0b
INSERT INTO sms_api_client (id, tenant_id, username, password_hash, email, authority, status, create_time, update_time) VALUES
  (1, '0b2f6a1e-4c55-4c8e-9d1a-000000000a0a', 'client_a',   '$2a$04$M5SdimAFbLZFmdnfPgDdv.mdg2JP8Wt5xAUCIsOA19PBH.GhOlnpm', 'client_a@example.invalid',   'API_CLIENT', 'ON',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  (2, '0b2f6a1e-4c55-4c8e-9d1a-000000000a0a', 'client_a2',  '$2a$04$M5SdimAFbLZFmdnfPgDdv.mdg2JP8Wt5xAUCIsOA19PBH.GhOlnpm', 'client_a2@example.invalid',  'API_CLIENT', 'ON',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  (3, '0b2f6a1e-4c55-4c8e-9d1a-000000000a0a', 'viewer_a',   '$2a$04$M5SdimAFbLZFmdnfPgDdv.mdg2JP8Wt5xAUCIsOA19PBH.GhOlnpm', 'viewer_a@example.invalid',   'API_VIEWER', 'ON',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  (4, '0b2f6a1e-4c55-4c8e-9d1a-000000000a0a', 'admin_a',    '$2a$04$M5SdimAFbLZFmdnfPgDdv.mdg2JP8Wt5xAUCIsOA19PBH.GhOlnpm', 'admin_a@example.invalid',    'API_ADMIN',  'ON',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  (5, '0b2f6a1e-4c55-4c8e-9d1a-000000000a0a', 'disabled_a', '$2a$04$M5SdimAFbLZFmdnfPgDdv.mdg2JP8Wt5xAUCIsOA19PBH.GhOlnpm', 'disabled_a@example.invalid', 'API_CLIENT', 'OFF', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  (6, '0b2f6a1e-4c55-4c8e-9d1a-000000000b0b', 'client_b',   '$2a$04$M5SdimAFbLZFmdnfPgDdv.mdg2JP8Wt5xAUCIsOA19PBH.GhOlnpm', 'client_b@example.invalid',   'API_CLIENT', 'ON',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO sms_provider (id, tenant_id, name, type, object_type, config_sealed, config_public, status, retention_days, create_time, update_time) VALUES
  (1, '0b2f6a1e-4c55-4c8e-9d1a-000000000a0a', 'mock-a',     'voicecom', 'OBJECT_TYPE_SMS', '\x00', '{"sid": "9999", "encoding": "utf-8", "priority": "2"}', 'ON',  0,  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  (2, '0b2f6a1e-4c55-4c8e-9d1a-000000000a0a', 'mock-a-off', 'voicecom', 'OBJECT_TYPE_SMS', '\x00', '{"sid": "9999", "encoding": "utf-8", "priority": "2"}', 'OFF', 0,  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  (3, '0b2f6a1e-4c55-4c8e-9d1a-000000000b0b', 'mock-b',     'voicecom', 'OBJECT_TYPE_SMS', '\x00', '{"sid": "8888", "encoding": "utf-8", "priority": "2"}', 'ON',  30, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO sms_template (id, tenant_id, name, object_type, templates, status, create_time, update_time) VALUES
  (1, '0b2f6a1e-4c55-4c8e-9d1a-000000000a0a', 'hello',  'OBJECT_TYPE_SMS', '{"body": "Hello {{ .name }}, code {{ .code }}."}', 'ON',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  (2, '0b2f6a1e-4c55-4c8e-9d1a-000000000a0a', 'static', 'OBJECT_TYPE_SMS', '{"body": "ping"}',                                 'ON',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  (3, '0b2f6a1e-4c55-4c8e-9d1a-000000000a0a', 'off',    'OBJECT_TYPE_SMS', '{"body": "off"}',                                  'OFF', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  (4, '0b2f6a1e-4c55-4c8e-9d1a-000000000b0b', 'hello',  'OBJECT_TYPE_SMS', '{"body": "Hi {{ .name }}"}',                       'ON',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- Provider-scoped, all-provider (legacy provider 0) and disabled blocks.
INSERT INTO sms_block (id, tenant_id, recipient, description, provider_id, block_type, status, created_by, create_time, update_time) VALUES
  (1, '0b2f6a1e-4c55-4c8e-9d1a-000000000a0a', '359888000301', 'fixture', 1,    'OBJECT_TYPE_SMS', 'ON',  'seed', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  (2, '0b2f6a1e-4c55-4c8e-9d1a-000000000a0a', '359888000302', 'fixture', NULL, 'OBJECT_TYPE_SMS', 'ON',  'seed', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  (3, '0b2f6a1e-4c55-4c8e-9d1a-000000000a0a', '359888000303', 'fixture', 1,    'OBJECT_TYPE_SMS', 'OFF', 'seed', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  (4, '0b2f6a1e-4c55-4c8e-9d1a-000000000b0b', '359888000301', 'fixture', NULL, 'OBJECT_TYPE_SMS', 'ON',  'seed', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- Messages accepted by the carrier (status 0), ready for receipt sequences.
INSERT INTO sms_message (id, tenant_id, actor_kind, api_client_id, platform_actor, sid, recipient, priority, provider_id, template_id, data,
                         status_code, message, status_message, create_time, update_time) VALUES
  ('00000000-0000-4000-8000-00000000000a', '0b2f6a1e-4c55-4c8e-9d1a-000000000a0a', 'api_client', 1, NULL, 9999, '359888000100', 2, 1, 2,
   '{"sms": {"from": "Fixture", "encoding": "utf-8"}, "properties": {}}', 0, 'ping', 'sms_provider_accepted', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('00000000-0000-4000-8000-00000000000c', '0b2f6a1e-4c55-4c8e-9d1a-000000000a0a', 'platform', NULL, '4f0c0a52-1111-4222-8333-944455556666', 9999, '359888000101', 2, 1, 2,
   '{"sms": {"from": "Fixture"}, "properties": {}}', 0, 'ping', 'sms_provider_accepted', '2026-01-02T00:01:00Z', '2026-01-02T00:01:00Z'),
  ('00000000-0000-4000-8000-00000000000b', '0b2f6a1e-4c55-4c8e-9d1a-000000000b0b', 'api_client', 6, NULL, 8888, '359888000200', 2, 3, 4,
   '{"sms": {"from": "Fixture"}, "properties": {"name": "Bo"}}', 0, 'Hi Bo', 'sms_provider_accepted', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z');

INSERT INTO sms_dlr (id, tenant_id, message_id, channel, sid, status_text, message_status, recipient, sender, "timestamp", remote_address, parts_received, create_time, update_time) VALUES
  (1, '0b2f6a1e-4c55-4c8e-9d1a-000000000a0a', '00000000-0000-4000-8000-00000000000a', 'sms', 9999, 'sms_smsc_delivered', 8, 359888000100, 'Fixture', 1700000000, '192.0.2.10', 1,
   '2026-01-02T00:00:05Z', '2026-01-02T00:00:05Z');

INSERT INTO sms_login_log (tenant_id, client_id, username, event_time, success, error_message, login_ip, user_agent) VALUES
  ('0b2f6a1e-4c55-4c8e-9d1a-000000000a0a', 1, 'client_a', '2026-01-01T12:00:00Z', true, '', '192.0.2.10', 'fixture'),
  (NULL, NULL, 'nobody', '2026-01-01T12:01:00Z', false, 'unknown user', '192.0.2.11', 'fixture');
