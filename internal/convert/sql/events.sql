-- Events: one GDELT 2.0 event export file (61 tab-separated columns, no header) to Parquet.
-- Placeholders, filled in by convert.go: {{csv}} input file, {{out}} output file, {{translated}} true/false.
-- Everything is read as text first and cast here with TRY_CAST, so an odd value becomes NULL instead of losing the row.
COPY (
    WITH raw AS (
        SELECT * FROM read_csv('{{csv}}',
            delim = '\t', header = false, quote = '', escape = '', auto_detect = false,
            ignore_errors = true, strict_mode = false, max_line_size = 100000000,
            columns = {
                'global_event_id': 'VARCHAR', 'sql_date': 'VARCHAR', 'month_year': 'VARCHAR', 'year': 'VARCHAR',
                'fraction_date': 'VARCHAR',
                'actor1_code': 'VARCHAR', 'actor1_name': 'VARCHAR', 'actor1_country_code': 'VARCHAR',
                'actor1_known_group_code': 'VARCHAR', 'actor1_ethnic_code': 'VARCHAR',
                'actor1_religion1_code': 'VARCHAR', 'actor1_religion2_code': 'VARCHAR', 'actor1_type1_code': 'VARCHAR',
                'actor1_type2_code': 'VARCHAR', 'actor1_type3_code': 'VARCHAR',
                'actor2_code': 'VARCHAR', 'actor2_name': 'VARCHAR', 'actor2_country_code': 'VARCHAR',
                'actor2_known_group_code': 'VARCHAR', 'actor2_ethnic_code': 'VARCHAR',
                'actor2_religion1_code': 'VARCHAR', 'actor2_religion2_code': 'VARCHAR', 'actor2_type1_code': 'VARCHAR',
                'actor2_type2_code': 'VARCHAR', 'actor2_type3_code': 'VARCHAR',
                'is_root_event': 'VARCHAR', 'event_code': 'VARCHAR', 'event_base_code': 'VARCHAR',
                'event_root_code': 'VARCHAR', 'quad_class': 'VARCHAR', 'goldstein_scale': 'VARCHAR',
                'num_mentions': 'VARCHAR', 'num_sources': 'VARCHAR', 'num_articles': 'VARCHAR', 'avg_tone': 'VARCHAR',
                'actor1_geo_type': 'VARCHAR', 'actor1_geo_full_name': 'VARCHAR', 'actor1_geo_country_code': 'VARCHAR',
                'actor1_geo_adm1_code': 'VARCHAR', 'actor1_geo_adm2_code': 'VARCHAR', 'actor1_geo_lat': 'VARCHAR',
                'actor1_geo_long': 'VARCHAR', 'actor1_geo_feature_id': 'VARCHAR',
                'actor2_geo_type': 'VARCHAR', 'actor2_geo_full_name': 'VARCHAR', 'actor2_geo_country_code': 'VARCHAR',
                'actor2_geo_adm1_code': 'VARCHAR', 'actor2_geo_adm2_code': 'VARCHAR', 'actor2_geo_lat': 'VARCHAR',
                'actor2_geo_long': 'VARCHAR', 'actor2_geo_feature_id': 'VARCHAR',
                'action_geo_type': 'VARCHAR', 'action_geo_full_name': 'VARCHAR', 'action_geo_country_code': 'VARCHAR',
                'action_geo_adm1_code': 'VARCHAR', 'action_geo_adm2_code': 'VARCHAR', 'action_geo_lat': 'VARCHAR',
                'action_geo_long': 'VARCHAR', 'action_geo_feature_id': 'VARCHAR',
                'date_added': 'VARCHAR', 'source_url': 'VARCHAR'
            })
    )
    SELECT
        TRY_CAST(global_event_id AS BIGINT) AS global_event_id,
        TRY_STRPTIME(sql_date, '%Y%m%d')::DATE AS sql_date,
        TRY_CAST(month_year AS INTEGER) AS month_year,
        TRY_CAST(year AS SMALLINT) AS year,
        TRY_CAST(fraction_date AS DOUBLE) AS fraction_date,
        actor1_code, actor1_name, actor1_country_code, actor1_known_group_code, actor1_ethnic_code,
        actor1_religion1_code, actor1_religion2_code, actor1_type1_code, actor1_type2_code, actor1_type3_code,
        actor2_code, actor2_name, actor2_country_code, actor2_known_group_code, actor2_ethnic_code,
        actor2_religion1_code, actor2_religion2_code, actor2_type1_code, actor2_type2_code, actor2_type3_code,
        is_root_event = '1' AS is_root_event,
        event_code, event_base_code, event_root_code,
        TRY_CAST(quad_class AS TINYINT) AS quad_class,
        TRY_CAST(goldstein_scale AS DOUBLE) AS goldstein_scale,
        TRY_CAST(num_mentions AS INTEGER) AS num_mentions,
        TRY_CAST(num_sources AS INTEGER) AS num_sources,
        TRY_CAST(num_articles AS INTEGER) AS num_articles,
        TRY_CAST(avg_tone AS DOUBLE) AS avg_tone,
        TRY_CAST(actor1_geo_type AS TINYINT) AS actor1_geo_type, actor1_geo_full_name, actor1_geo_country_code,
        actor1_geo_adm1_code, actor1_geo_adm2_code,
        TRY_CAST(actor1_geo_lat AS DOUBLE) AS actor1_geo_lat, TRY_CAST(actor1_geo_long AS DOUBLE) AS actor1_geo_long,
        actor1_geo_feature_id,
        TRY_CAST(actor2_geo_type AS TINYINT) AS actor2_geo_type, actor2_geo_full_name, actor2_geo_country_code,
        actor2_geo_adm1_code, actor2_geo_adm2_code,
        TRY_CAST(actor2_geo_lat AS DOUBLE) AS actor2_geo_lat, TRY_CAST(actor2_geo_long AS DOUBLE) AS actor2_geo_long,
        actor2_geo_feature_id,
        TRY_CAST(action_geo_type AS TINYINT) AS action_geo_type, action_geo_full_name, action_geo_country_code,
        action_geo_adm1_code, action_geo_adm2_code,
        TRY_CAST(action_geo_lat AS DOUBLE) AS action_geo_lat, TRY_CAST(action_geo_long AS DOUBLE) AS action_geo_long,
        action_geo_feature_id,
        TRY_STRPTIME(date_added, '%Y%m%d%H%M%S') AS date_added,
        source_url,
        {{translated}} AS translated
    FROM raw
) TO '{{out}}' (FORMAT parquet, COMPRESSION zstd);
