-- Representative legacy (v3) sms-gw snapshot for the migration tests and the
-- T052 rehearsal: every entity type over the captured legacy schema
-- (runtime/legacy-schema.sql). Values are synthetic; the carrier token, the
-- receipt token and the callback secret are fixture-only. Password of every
-- client: Passw0rd!. Ids have gaps on purpose (imports preserve them).
--
-- Raw evidence is gzip-compressed like the legacy service stored it; message
-- ...0101 carries the carrier token and the dlr_token in clear.

INSERT INTO public.sms_api_client (id, create_time, update_time, username, password_hash, email, authority, status, last_login_time, last_login_ip, dlr_callback_url, dlr_callback_secret) VALUES
  (1, '2025-01-02T03:04:05.123456Z', '2025-02-01T00:00:00Z', 'client_a', '$2a$04$M5SdimAFbLZFmdnfPgDdv.mdg2JP8Wt5xAUCIsOA19PBH.GhOlnpm', 'a@example.invalid', 'API_CLIENT', 'ON', '2025-03-01T10:00:00.5Z', '198.51.100.7', 'https://hooks.example.com/a', 'snapshot-callback-secret-not-production'),
  (2, '2025-01-02T03:04:06Z', '2025-01-02T03:04:06Z', 'client_b', '$2a$04$M5SdimAFbLZFmdnfPgDdv.mdg2JP8Wt5xAUCIsOA19PBH.GhOlnpm', NULL, 'API_CLIENT', 'ON', NULL, '', 'https://hooks.example.com/b', ''),
  (3, '2025-01-02T03:04:07Z', '2025-01-02T03:04:07Z', 'viewer_v', '$2a$04$M5SdimAFbLZFmdnfPgDdv.mdg2JP8Wt5xAUCIsOA19PBH.GhOlnpm', 'v@example.invalid', 'API_VIEWER', 'ON', NULL, '', '', ''),
  (4, '2025-01-02T03:04:08Z', '2025-01-02T03:04:08Z', 'admin_x', '$2a$04$M5SdimAFbLZFmdnfPgDdv.mdg2JP8Wt5xAUCIsOA19PBH.GhOlnpm', '', 'API_ADMIN', 'ON', NULL, '', '', ''),
  (7, '2025-01-02T03:04:09Z', '2025-04-01T00:00:00Z', 'disabled_d', '$2a$04$M5SdimAFbLZFmdnfPgDdv.mdg2JP8Wt5xAUCIsOA19PBH.GhOlnpm', '', 'API_CLIENT', 'OFF', '2025-02-01T00:00:00Z', '203.0.113.9', '', '');

INSERT INTO public.sms_provider (id, create_time, update_time, name, type, object_type, config, status, retention_days) VALUES
  (10, '2025-01-01T00:00:00Z', '2025-01-05T00:00:00Z', 'voicecom-main', 'voicecom', 'OBJECT_TYPE_SMS',
   '{"url": "https://carrier.invalid/multichannel-api/sendmulti/", "sid": "9999", "token": "snapshot-carrier-token-not-production", "dlr_token": "snapshotdlrtoken0123456789abcdef", "callback_url": "https://gw.example.com/dlr?dlr_token=snapshotdlrtoken0123456789abcdef", "encoding": "utf-8", "priority": "2"}',
   'ON', 30),
  (11, '2025-01-01T00:00:01Z', '2025-01-01T00:00:01Z', 'voicecom-legacy-no-token', 'voicecom', 'OBJECT_TYPE_SMS',
   '{"url": "https://carrier.invalid/multichannel-api/sendmulti/", "sid": "9998", "callback_url": "https://gw.example.com/dlr"}', 'OFF', 0),
  (12, '2025-01-01T00:00:02Z', '2025-01-01T00:00:02Z', 'viber-main', 'voicecom', 'OBJECT_TYPE_VIBER',
   '{"url": "https://carrier.invalid/multichannel-api/sendmulti/", "sid": "9997"}', 'ON', 0),
  (13, '2025-01-01T00:00:03Z', '2025-01-01T00:00:03Z', 'unknown-type', 'carrier-x', 'OBJECT_TYPE_SMS', NULL, 'ON', 7);

INSERT INTO public.sms_template (id, create_time, update_time, name, object_type, templates, status) VALUES
  (20, '2025-01-01T00:00:00Z', '2025-01-01T00:00:00Z', 'hello', 'OBJECT_TYPE_SMS', '{"body": "Hello {{ .name }}, code {{ .code }}."}', 'ON'),
  (21, '2025-01-01T00:00:00Z', '2025-01-01T00:00:00Z', 'unicode', 'OBJECT_TYPE_SMS', '{"body": "Здравей {{ .name }}", "subject": "unused"}', 'ON'),
  (22, '2025-01-01T00:00:00Z', '2025-01-01T00:00:00Z', 'off', 'OBJECT_TYPE_SMS', '{"body": "off"}', 'OFF'),
  (23, '2025-01-01T00:00:00Z', '2025-01-01T00:00:00Z', 'viber', 'OBJECT_TYPE_VIBER', '{"body": "viber"}', 'ON');

-- provider_id 0 is the legacy "every provider"; 33 carries a delete_time the
-- legacy service never enforced.
INSERT INTO public.sms_block (id, create_by, create_time, update_time, delete_time, recipient, description, provider_id, block_type, status) VALUES
  (30, 5, '2025-01-03T00:00:00Z', '2025-01-03T00:00:00Z', NULL, '359888000301', 'provider block', 10, 'OBJECT_TYPE_SMS', 'ON'),
  (31, NULL, '2025-01-03T00:00:00Z', '2025-01-03T00:00:00Z', NULL, '359888000302', 'global block', 0, 'OBJECT_TYPE_SMS', 'ON'),
  (32, 0, '2025-01-03T00:00:00Z', '2025-01-03T00:00:00Z', NULL, '359888000303', NULL, 10, 'OBJECT_TYPE_SMS', 'OFF'),
  (33, 5, '2025-01-03T00:00:00Z', '2025-01-03T00:00:00Z', '2025-01-04T00:00:00Z', '359888000304', 'soft deleted', NULL, 'OBJECT_TYPE_SMS', 'ON');

-- create_by is the owning API client; 0 and NULL are legacy admin sends.
INSERT INTO public.sms_message (id, create_by, create_time, update_time, sid, recipient, priority, provider_id, template_id, defer, user_name, raw_response, raw_request, data, dlr_ts, status_code, message, status_message, remote_address) VALUES
  ('00000000-0000-4000-8000-000000000101', 1, '2025-03-01T10:00:01.234567Z', '2025-03-01T10:00:05Z', 9999, '359888000100', 2, 10, 20, '', 'client_a',
   decode('1f8b0800000000000203f3080909d037d4335430323050f0f7e6e572cecf2b49cd2bd10da92c48b552482c28c8c94c4e2cc9cccfd3cf2acecfe3e5e2e5aa562a4a2d292dca8b4fce4f4955b232d001f20b4b538b4be2335394ac940ca040174c9880080b18170c0c0d0c956a010ca270a778000000', 'hex'),
   decode('1f8b0800000000000203358fcd6ac3301084ef06bf83d039b6e4fc34b62184d28b6f0dd4f7b091d4588dbc1292dc524adfbdb27117f630dfc0eccee5f5ad276c9c4cd4620044650a709a058572818c747d7f615559e55967436c8900efb5f2a5c64f305ae6d98bc5a83016fdb7532d01e78c1610b545f6112ce6599efd5001c6dc403cae9337b4a5438cae65ecfef59fc2a4f1e7b4d7681f0a4f01c185c1c64416c0abed6e7f783ad60ddc8454ef7443d76f53581843d2a30a01ee2ae94e1963c933c286082b15d96fcbe4bbc1e2ecee0e4d5dd79cf38af384839609366992586ecd89ebf962ed5a2c468189386fe524e676f4f70f0b172aa03c010000', 'hex'),
   '{"sms": {"from": "Snapshot", "encoding": "utf-8"}, "properties": {"name": "Ana", "code": "42"}}', 1740823205, 1, 'Hello Ana, code 42.', 'sms_delivered', '198.51.100.7'),
  ('00000000-0000-4000-8000-000000000102', 1, '2025-03-01T10:01:00Z', '2025-03-01T10:01:00Z', 9999, '359888000101', 2, 10, 0, '', 'client_a', NULL, NULL, '{"sms": {}}', 0, 0, 'ping', 'sms_provider_accepted', '198.51.100.7'),
  ('00000000-0000-4000-8000-000000000103', 0, '2025-03-01T10:02:00Z', '2025-03-01T10:02:00Z', 9999, '359888000102', 2, 10, 21, '', '', NULL, NULL, NULL, 0, 0, 'Здравей Admin', 'sms_provider_accepted', ''),
  ('00000000-0000-4000-8000-000000000104', 2, '2025-03-02T00:00:00Z', '2025-03-02T00:00:01Z', 9999, '359888000103', 2, 10, 20, '', 'client_b', NULL, NULL, '{"sms": {}}', 0, 500, 'Hello , code .', 'voicecom: carrier returned HTTP 500', '203.0.113.20'),
  ('00000000-0000-4000-8000-000000000105', 2, '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z', 9999, '359888000104', 2, 11, 0, '', 'client_b', NULL, NULL, NULL, 0, -1, 'old', 'Message arrived at sms-gw', ''),
  ('00000000-0000-4000-8000-000000000106', NULL, '2025-03-03T00:00:00Z', '2025-03-03T00:00:00Z', 0, '359888000105', 2, 13, 0, '', '', NULL, NULL, NULL, 0, 2001, 'admin', 'sms_invalid_sid', '');

INSERT INTO public.sms_dlr (id, create_time, update_time, channel, sid, status_text, message_status, recipient, sender, "timestamp", remote_address, parts_received, message_id) VALUES
  (40, '2025-03-01T10:00:03Z', '2025-03-01T10:00:03Z', 'sms', 9999, 'sms_provider_accepted', 0, 359888000100, 'Snapshot', 1740823203, '192.0.2.50', 1, '00000000-0000-4000-8000-000000000101'),
  (42, '2025-03-01T10:00:05Z', '2025-03-01T10:00:06Z', 'sms', 9999, 'sms_delivered', 1, 359888000100, 'Snapshot', 1740823205, '192.0.2.50', 2, '00000000-0000-4000-8000-000000000101'),
  (43, '2025-03-01T10:02:05Z', '2025-03-01T10:02:05Z', 'sms', 9999, NULL, 77, 359888000102, '', 1740823325, '192.0.2.50', 1, '00000000-0000-4000-8000-000000000103');

-- client_id 0/NULL: the username did not resolve; 9 is a deleted client.
INSERT INTO public.sms_login_log (id, client_id, username, event_time, success, error_message, login_ip, user_agent) VALUES
  (50, 1, 'client_a', '2025-03-01T09:59:59Z', true, '', '198.51.100.7', 'curl/8.0'),
  (51, 2, 'client_b', '2025-03-01T10:00:00Z', false, 'invalid credentials', '203.0.113.20', ''),
  (52, 0, 'nobody', '2025-03-01T10:00:01Z', false, 'invalid credentials', '203.0.113.21', 'probe/1.0'),
  (53, NULL, 'ghost', '2025-03-01T10:00:02Z', false, 'invalid credentials', '203.0.113.22', NULL),
  (54, 9, 'removed_c', '2025-03-01T10:00:03Z', true, '', '203.0.113.23', '');

SELECT setval('public.sms_api_client_id_seq', 7), setval('public.sms_provider_id_seq', 13), setval('public.sms_template_id_seq', 23),
  setval('public.sms_block_id_seq', 33), setval('public.sms_dlr_id_seq', 43), setval('public.sms_login_log_id_seq', 54);
