-- Mentions: one GDELT 2.0 mentions file (16 tab-separated columns, no header) to Parquet.
-- Placeholders, filled in by convert.go: {{csv}} input file, {{out}} output file, {{translated}} true/false.
COPY (
    WITH raw AS (
        SELECT * FROM read_csv('{{csv}}',
            delim = '\t', header = false, quote = '', escape = '', auto_detect = false,
            ignore_errors = true, strict_mode = false, max_line_size = 100000000,
            columns = {
                'global_event_id': 'VARCHAR', 'event_time_date': 'VARCHAR', 'mention_time_date': 'VARCHAR',
                'mention_type': 'VARCHAR', 'mention_source_name': 'VARCHAR', 'mention_identifier': 'VARCHAR',
                'sentence_id': 'VARCHAR', 'actor1_char_offset': 'VARCHAR', 'actor2_char_offset': 'VARCHAR',
                'action_char_offset': 'VARCHAR', 'in_raw_text': 'VARCHAR', 'confidence': 'VARCHAR',
                'mention_doc_len': 'VARCHAR', 'mention_doc_tone': 'VARCHAR', 'mention_doc_translation_info': 'VARCHAR',
                'extras': 'VARCHAR'
            })
    )
    SELECT
        TRY_CAST(global_event_id AS BIGINT) AS global_event_id,
        TRY_STRPTIME(event_time_date, '%Y%m%d%H%M%S') AS event_time,
        TRY_STRPTIME(mention_time_date, '%Y%m%d%H%M%S') AS mention_time,
        TRY_CAST(mention_type AS TINYINT) AS mention_type,
        mention_source_name,
        mention_identifier,
        TRY_CAST(sentence_id AS INTEGER) AS sentence_id,
        TRY_CAST(actor1_char_offset AS INTEGER) AS actor1_char_offset,
        TRY_CAST(actor2_char_offset AS INTEGER) AS actor2_char_offset,
        TRY_CAST(action_char_offset AS INTEGER) AS action_char_offset,
        in_raw_text = '1' AS in_raw_text,
        TRY_CAST(confidence AS TINYINT) AS confidence,
        TRY_CAST(mention_doc_len AS INTEGER) AS mention_doc_len,
        TRY_CAST(mention_doc_tone AS DOUBLE) AS mention_doc_tone,
        mention_doc_translation_info,
        extras,
        {{translated}} AS translated
    FROM raw
) TO '{{out}}' (FORMAT parquet, COMPRESSION zstd);
