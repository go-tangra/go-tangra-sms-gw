-- Parity seed, applied to the legacy (v3) database after the legacy service
-- created its schema; V4 receives the same rows through cmd/smsgw-migrate.
-- Shaped like production: API_CLIENT ids 1-3, voicecom providers 1, 4, 5,
-- 10, 11 (callback without dlr_token, as in production, plus one with a
-- token), 13 templates are reduced to the cases the validation order needs.
-- Every password is Passw0rd! (hash supplied by the harness as :hash).
-- :carrier is the mock carrier base URL, :fake the failure-mode fake.

INSERT INTO sms_api_client (id, create_time, update_time, username, password_hash, email, authority, status, last_login_ip, dlr_callback_url, dlr_callback_secret) VALUES
  (1, '2025-01-02T03:04:05Z', '2025-01-02T03:04:05Z', 'client_a', ':hash', 'a@example.invalid', 'API_CLIENT', 'ON', '', '', ''),
  (2, '2025-01-02T03:04:06Z', '2025-01-02T03:04:06Z', 'client_b', ':hash', '', 'API_CLIENT', 'ON', '', '', ''),
  (3, '2025-01-02T03:04:07Z', '2025-01-02T03:04:07Z', 'client_c', ':hash', 'c@example.invalid', 'API_CLIENT', 'ON', '', '', ''),
  (5, '2025-01-02T03:04:08Z', '2025-01-02T03:04:08Z', 'viewer_v', ':hash', 'v@example.invalid', 'API_VIEWER', 'ON', '', '', ''),
  (6, '2025-01-02T03:04:09Z', '2025-01-02T03:04:09Z', 'admin_x', ':hash', '', 'API_ADMIN', 'ON', '', '', ''),
  (7, '2025-01-02T03:04:10Z', '2025-01-02T03:04:10Z', 'disabled_d', ':hash', '', 'API_CLIENT', 'OFF', '', '', '');

INSERT INTO sms_provider (id, create_time, update_time, name, type, object_type, config, status, retention_days) VALUES
  (1, '2025-01-01T00:00:00Z', '2025-01-01T00:00:00Z', 'voicecom-token', 'voicecom', 'OBJECT_TYPE_SMS',
   '{"url": ":carrier/multichannel-api/sendmulti/", "sid": "9999", "token": "parity-carrier-token", "dlr_token": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "callback_url": "https://sms-gw.example.invalid/dlr?dlr_token=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "encoding": "utf-8", "priority": "2"}', 'ON', 0),
  (4, '2025-01-01T00:00:01Z', '2025-01-01T00:00:01Z', 'voicecom-prod-like', 'voicecom', 'OBJECT_TYPE_SMS',
   '{"url": ":carrier/multichannel-api/sendmulti/", "sid": "9998", "token": "parity-carrier-token", "callback_url": "https://sms-gw.example.invalid/hermes/v1/sms/dlr", "encoding": "utf-8", "priority": "1"}', 'ON', 0),
  (5, '2025-01-01T00:00:02Z', '2025-01-01T00:00:02Z', 'voicecom-off', 'voicecom', 'OBJECT_TYPE_SMS',
   '{"url": ":carrier/multichannel-api/sendmulti/", "sid": "9997", "callback_url": "https://sms-gw.example.invalid/hermes/v1/sms/dlr"}', 'OFF', 0),
  (10, '2025-01-01T00:00:03Z', '2025-01-01T00:00:03Z', 'voicecom-reject', 'voicecom', 'OBJECT_TYPE_SMS',
   '{"url": ":fake/reject", "sid": "9996", "encoding": "utf-8", "callback_url": "https://sms-gw.example.invalid/hermes/v1/sms/dlr"}', 'ON', 0),
  (11, '2025-01-01T00:00:04Z', '2025-01-01T00:00:04Z', 'voicecom-http500', 'voicecom', 'OBJECT_TYPE_SMS',
   '{"url": ":fake/http500", "sid": "9995", "encoding": "utf-8", "callback_url": "https://sms-gw.example.invalid/hermes/v1/sms/dlr"}', 'ON', 0),
  (12, '2025-01-01T00:00:05Z', '2025-01-01T00:00:05Z', 'viber', 'voicecom', 'OBJECT_TYPE_VIBER',
   '{"url": ":carrier/multichannel-api/sendmulti/", "sid": "9994"}', 'ON', 0);

INSERT INTO sms_template (id, create_time, update_time, name, object_type, templates, status) VALUES
  (1, '2025-01-01T00:00:00Z', '2025-01-01T00:00:00Z', 'hello', 'OBJECT_TYPE_SMS', '{"body": "Hello {{ .name }}, code {{ .code }}."}', 'ON'),
  (2, '2025-01-01T00:00:00Z', '2025-01-01T00:00:00Z', 'static', 'OBJECT_TYPE_SMS', '{"body": "ping"}', 'ON'),
  (3, '2025-01-01T00:00:00Z', '2025-01-01T00:00:00Z', 'off', 'OBJECT_TYPE_SMS', '{"body": "off"}', 'OFF'),
  (4, '2025-01-01T00:00:00Z', '2025-01-01T00:00:00Z', 'viber', 'OBJECT_TYPE_VIBER', '{"body": "viber"}', 'ON'),
  (5, '2025-01-01T00:00:00Z', '2025-01-01T00:00:00Z', 'nobody', 'OBJECT_TYPE_SMS', '{"subject": "no body"}', 'ON'),
  (6, '2025-01-01T00:00:00Z', '2025-01-01T00:00:00Z', 'badsyntax', 'OBJECT_TYPE_SMS', '{"body": "Hello {{ .name"}', 'ON'),
  (7, '2025-01-01T00:00:00Z', '2025-01-01T00:00:00Z', 'unicode', 'OBJECT_TYPE_SMS', '{"body": "Здравей {{ .name }} / <b>&amp;</b>"}', 'ON'),
  (13, '2025-01-01T00:00:00Z', '2025-01-01T00:00:00Z', 'otp', 'OBJECT_TYPE_SMS', '{"body": "Your code is {{ .code }}"}', 'ON');

INSERT INTO sms_block (id, create_by, create_time, update_time, recipient, description, provider_id, block_type, status) VALUES
  (30, 0, '2025-01-03T00:00:00Z', '2025-01-03T00:00:00Z', '359888000301', 'provider block', 1, 'OBJECT_TYPE_SMS', 'ON'),
  (31, 0, '2025-01-03T00:00:00Z', '2025-01-03T00:00:00Z', '359888000302', 'global block', 0, 'OBJECT_TYPE_SMS', 'ON'),
  (32, 0, '2025-01-03T00:00:00Z', '2025-01-03T00:00:00Z', '359888000303', 'disabled block', 1, 'OBJECT_TYPE_SMS', 'OFF');

-- Historical messages with legacy (version 4) ids, as imported in production.
INSERT INTO sms_message (id, create_by, create_time, update_time, sid, recipient, priority, provider_id, template_id, defer, user_name, raw_response, raw_request, data, dlr_ts, status_code, message, status_message, remote_address) VALUES
  ('00000000-0000-4000-8000-000000000101', 1, '2025-03-01T10:00:01Z', '2025-03-01T10:00:05Z', 9999, '359888000100', 2, 1, 1, '', 'client_a', NULL, NULL,
   '{"sms": {"from": "Parity", "encoding": "utf-8"}, "properties": {"name": "Ana", "code": "42"}}', 1740823205, 1, 'Hello Ana, code 42.', 'sms_delivered', '198.51.100.7'),
  ('00000000-0000-4000-8000-000000000102', 1, '2025-03-01T10:01:00Z', '2025-03-01T10:01:00Z', 9999, '359888000101', 2, 1, 2, '', 'client_a', NULL, NULL,
   '{"sms": {"from": "Parity"}, "properties": {}}', 0, 0, 'ping', 'sms_provider_accepted', '198.51.100.7'),
  ('00000000-0000-4000-8000-000000000103', 2, '2025-03-02T00:00:00Z', '2025-03-02T00:00:00Z', 9998, '359888000102', 1, 4, 2, '', 'client_b', NULL, NULL,
   '{"sms": {}}', 0, 0, 'ping', 'sms_provider_accepted', '203.0.113.20'),
  ('00000000-0000-4000-8000-000000000104', 1, '2025-03-03T00:00:00Z', '2025-03-03T00:00:00Z', 9998, '359888000103', 1, 4, 2, '', 'client_a', NULL, NULL,
   '{"sms": {}}', 0, 0, 'ping', 'sms_provider_accepted', '198.51.100.7');

INSERT INTO sms_dlr (id, create_time, update_time, channel, sid, status_text, message_status, recipient, sender, "timestamp", remote_address, parts_received, message_id) VALUES
  (40, '2025-03-01T10:00:03Z', '2025-03-01T10:00:03Z', 'sms', 9999, 'sms_provider_accepted', 0, 359888000100, 'Parity', 1740823203, '192.0.2.50', 1, '00000000-0000-4000-8000-000000000101'),
  (42, '2025-03-01T10:00:05Z', '2025-03-01T10:00:06Z', 'sms', 9999, 'sms_delivered', 1, 359888000100, 'Parity', 1740823205, '192.0.2.50', 2, '00000000-0000-4000-8000-000000000101');

SELECT setval('sms_api_client_id_seq', 7), setval('sms_provider_id_seq', 12), setval('sms_template_id_seq', 13),
  setval('sms_block_id_seq', 32), setval('sms_dlr_id_seq', 42);
