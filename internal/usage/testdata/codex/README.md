Synthetic collector/cache output contract for issue #72. No provider capture,
real credentials or account identifiers. The Go test compares the serialized
card produced by mocked collection and real Store retention to this fixture;
the Node renderer test consumes that same card. Its fixed original timestamp
proves that repeated failed attempts do not make old evidence appear fresh.
