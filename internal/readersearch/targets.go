package readersearch

import "strings"

// All expressions are host constants, never SQL supplied by a book or caller.
// Shared by fan-out and stale-result filtering to keep invalidation rules aligned.
var searchTables = []string{"reader_book", "reader_book_title", "reader_book_lifecycle", "reader_annotation", "reader_annotation_lifecycle", "reader_edit_session", "reader_stroke", "reader_erase_claim", "reader_annotation_value", "reader_recognition"}

func affects(table, key string) string {
	switch table {
	case "reader_book", "reader_book_title", "reader_book_lifecycle":
		return "a.book_id=" + key
	case "reader_annotation", "reader_annotation_lifecycle":
		return "a.id=" + key
	case "reader_edit_session", "reader_stroke", "reader_recognition":
		return "a.id=(SELECT annotation_id FROM fn_" + table + " WHERE id=" + key + ")"
	case "reader_annotation_value":
		return "a.id=(SELECT s.annotation_id FROM fn_reader_annotation_value v JOIN fn_reader_edit_session s ON s.id=v.session_id WHERE v.id=" + key + ")"
	case "reader_erase_claim":
		return "a.id=(SELECT s.annotation_id FROM fn_reader_erase_claim ec JOIN fn_reader_stroke s ON s.id=ec.stroke_id WHERE ec.id=" + key + ")"
	default:
		return "0" // Reading position and prepared reference graph are not annotation search documents.
	}
}
func dirty(revision string) string {
	var terms []string
	for _, table := range searchTables {
		terms = append(terms, "(c.table_name='"+table+"' AND "+affects(table, "c.pk")+")")
	}
	return "EXISTS(SELECT 1 FROM reader_store_changes c WHERE c.seq>" + revision + " AND (" + strings.Join(terms, " OR ") + "))"
}
