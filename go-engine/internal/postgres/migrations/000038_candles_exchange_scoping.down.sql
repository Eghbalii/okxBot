ALTER TABLE candles DROP CONSTRAINT candles_pkey;
ALTER TABLE candles ADD PRIMARY KEY (inst_id, bar, ts);
ALTER TABLE candles DROP COLUMN exchange;
