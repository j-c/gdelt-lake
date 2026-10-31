-- GKG: one GDELT 2.1 Global Knowledge Graph file (27 tab-separated columns, no header) to Parquet.
-- Placeholders, filled in by convert.go: {{csv}} input file, {{out}} output file, {{translated}} true/false.
--
-- The commonly used delimited fields are parsed into lists and structs (design A14): themes, locations, persons,
-- organisations, tone, translation info and the page title. GDELT's older V1 versions of those fields (and of counts)
-- are left out: the V2 versions hold the same values plus character offsets. GCAM is dropped (design R5). Every other
-- field is kept as GDELT's text.
COPY (
    WITH raw AS (
        SELECT * FROM read_csv('{{csv}}',
            delim = '\t', header = false, quote = '', escape = '', auto_detect = false,
            ignore_errors = true, strict_mode = false, max_line_size = 100000000,
            columns = {
                'gkg_record_id': 'VARCHAR', 'date': 'VARCHAR', 'source_collection_identifier': 'VARCHAR',
                'source_common_name': 'VARCHAR', 'document_identifier': 'VARCHAR',
                'counts': 'VARCHAR', 'v2_counts': 'VARCHAR', 'themes': 'VARCHAR', 'v2_themes': 'VARCHAR',
                'locations': 'VARCHAR', 'v2_locations': 'VARCHAR', 'persons': 'VARCHAR', 'v2_persons': 'VARCHAR',
                'organizations': 'VARCHAR', 'v2_organizations': 'VARCHAR', 'v2_tone': 'VARCHAR', 'dates': 'VARCHAR',
                'gcam': 'VARCHAR', 'sharing_image': 'VARCHAR', 'related_images': 'VARCHAR',
                'social_image_embeds': 'VARCHAR', 'social_video_embeds': 'VARCHAR', 'quotations': 'VARCHAR',
                'all_names': 'VARCHAR', 'amounts': 'VARCHAR', 'translation_info': 'VARCHAR', 'extras': 'VARCHAR'
            })
    ),
    -- Split "a;b;c;" lists into their non-empty items, keeping NULL for an empty field.
    split AS (
        SELECT *,
            list_filter(string_split(v2_themes, ';'), lambda x: x <> '') AS theme_items,
            list_filter(string_split(v2_locations, ';'), lambda x: x <> '') AS location_items,
            list_filter(string_split(v2_persons, ';'), lambda x: x <> '') AS person_items,
            list_filter(string_split(v2_organizations, ';'), lambda x: x <> '') AS organization_items,
            string_split(v2_tone, ',') AS tone_items
        FROM raw
    )
    SELECT
        gkg_record_id,
        TRY_STRPTIME(date, '%Y%m%d%H%M%S') AS date,
        TRY_CAST(source_collection_identifier AS TINYINT) AS source_collection_identifier,
        source_common_name,
        document_identifier,
        NULLIF(regexp_extract(extras, '<PAGE_TITLE>(.*?)</PAGE_TITLE>', 1), '') AS page_title,
        -- "THEME,offset"
        list_transform(theme_items, lambda x: {
            'theme': split_part(x, ',', 1),
            'char_offset': TRY_CAST(split_part(x, ',', 2) AS INTEGER)
        }) AS themes,
        -- "type#full name#country#ADM1#ADM2#lat#long#feature id#offset"
        list_transform(location_items, lambda x: {
            'type': TRY_CAST(string_split(x, '#')[1] AS TINYINT),
            'full_name': NULLIF(string_split(x, '#')[2], ''),
            'country_code': NULLIF(string_split(x, '#')[3], ''),
            'adm1_code': NULLIF(string_split(x, '#')[4], ''),
            'adm2_code': NULLIF(string_split(x, '#')[5], ''),
            'lat': TRY_CAST(string_split(x, '#')[6] AS DOUBLE),
            'long': TRY_CAST(string_split(x, '#')[7] AS DOUBLE),
            'feature_id': NULLIF(string_split(x, '#')[8], ''),
            'char_offset': TRY_CAST(string_split(x, '#')[9] AS INTEGER)
        }) AS locations,
        -- "Name,offset" (a name can itself contain commas, so the offset is whatever follows the last one)
        list_transform(person_items, lambda x: {
            'name': regexp_extract(x, '^(.*),(-?\d+)$', 1),
            'char_offset': TRY_CAST(regexp_extract(x, '^(.*),(-?\d+)$', 2) AS INTEGER)
        }) AS persons,
        list_transform(organization_items, lambda x: {
            'name': regexp_extract(x, '^(.*),(-?\d+)$', 1),
            'char_offset': TRY_CAST(regexp_extract(x, '^(.*),(-?\d+)$', 2) AS INTEGER)
        }) AS organizations,
        -- "tone,positive,negative,polarity,activity reference density,self/group reference density,word count"
        CASE WHEN v2_tone IS NULL THEN NULL ELSE {
            'tone': TRY_CAST(tone_items[1] AS DOUBLE),
            'positive_score': TRY_CAST(tone_items[2] AS DOUBLE),
            'negative_score': TRY_CAST(tone_items[3] AS DOUBLE),
            'polarity': TRY_CAST(tone_items[4] AS DOUBLE),
            'activity_reference_density': TRY_CAST(tone_items[5] AS DOUBLE),
            'self_group_reference_density': TRY_CAST(tone_items[6] AS DOUBLE),
            'word_count': TRY_CAST(tone_items[7] AS INTEGER)
        } END AS tone,
        -- "srclc:hrv;eng:GT-HRV 1.0", only in the translingual stream
        CASE WHEN translation_info IS NULL THEN NULL ELSE {
            'source_language': NULLIF(regexp_extract(translation_info, 'srclc:([^;]*)', 1), ''),
            'engine': NULLIF(regexp_extract(translation_info, 'eng:([^;]*)', 1), '')
        } END AS translation,
        v2_counts AS counts,
        dates,
        sharing_image,
        related_images,
        social_image_embeds,
        social_video_embeds,
        quotations,
        all_names,
        amounts,
        extras,
        {{translated}} AS translated
    FROM split
) TO '{{out}}' (FORMAT parquet, COMPRESSION zstd);
