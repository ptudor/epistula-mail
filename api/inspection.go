package main

import (
	"fmt"
	"net/http"
	"strconv"
)

// Inspection pages use stable part/model keys, independently of ranking.
// Each request reads current sidecars; concurrent additions before a cursor
// require restarting that traversal. Full-document/default ordering is intact.
// Large summaries have their own byte cursor, preserving every model and byte
// without retaining every sidecar or one unbounded summary in the API/MCP.
func (s *server) inspectionMetadata(r *http.Request, doc *messageDoc) error {
	const page = 4
	const summaryLimit = 16 * 1024
	afterAtt := r.URL.Query().Get("attachment_after")
	afterAnn := r.URL.Query().Get("annotation_after")
	model := r.URL.Query().Get("annotation_model")
	offset := int64(0)
	if raw := r.URL.Query().Get("summary_offset"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			return fmt.Errorf("invalid summary_offset")
		}
		offset = n
	}
	// Saturating arithmetic is unnecessary: SQL subtracts three and only ever
	// adds the fixed small window to a bounded substring length.
	base := max(offset-3, 0)
	doc.Attachments = []attachmentItem{}
	rows, err := s.pool.Query(r.Context(), `SELECT part_number,filename,content_type,content_id,disposition,size_bytes,encode(sha256,'hex')
 FROM attachments WHERE message_id=$1 AND part_number COLLATE "C">$2 COLLATE "C" ORDER BY part_number COLLATE "C" LIMIT 5`, doc.ID, afterAtt)
	if err != nil {
		return err
	}
	for rows.Next() {
		var a attachmentItem
		if err := rows.Scan(&a.PartNumber, &a.Filename, &a.ContentType, &a.ContentID, &a.Disposition, &a.SizeBytes, &a.SHA256); err != nil {
			rows.Close()
			return err
		}
		if len(doc.Attachments) == page {
			doc.NextAttachment = doc.Attachments[page-1].PartNumber
			break
		}
		doc.Attachments = append(doc.Attachments, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	rankSelect := `NULL::integer,false`
	rankJoin := ""
	if s.modelPriorityAvailable {
		rankSelect = `COALESCE(am.priority,0),COALESCE(a.model=(SELECT best.model FROM message_annotations best LEFT JOIN annotation_models bm ON bm.model=best.model
   WHERE best.message_id=$1 AND bm.retired_at IS NULL ORDER BY COALESCE(bm.priority,0) DESC,best.created_at DESC,best.model LIMIT 1),false)`
		rankJoin = `LEFT JOIN annotation_models am ON am.model=a.model`
	}
	query := `SELECT a.model,a.tags,a.category,
  substring(convert_to(a.summary,'UTF8') FROM LEAST($4::bigint,octet_length(a.summary)::bigint)::integer+1 FOR $5::integer),
  octet_length(a.summary)::bigint,a.created_at,` + rankSelect + `
  FROM message_annotations a ` + rankJoin + `
  WHERE a.message_id=$1 AND (($3='' AND a.model COLLATE "C">$2 COLLATE "C") OR a.model=$3)
  ORDER BY a.model COLLATE "C" LIMIT 5`
	rows, err = s.pool.Query(r.Context(), query, doc.ID, afterAnn, model, base, summaryLimit+7)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var a annotationItem
		var raw []byte
		var total *int64
		if err := rows.Scan(&a.Model, &a.Tags, &a.Category, &raw, &total, &a.CreatedAt, &a.Priority, &a.Primary); err != nil {
			return err
		}
		if len(doc.Annotations) == page {
			doc.NextAnnotation = doc.Annotations[page-1].Model
			break
		}
		if total != nil {
			start := min(offset, *total)
			// A summary offset applies only to the explicitly requested model.
			// Ordinary model-page requests always begin every summary at zero.
			relative := int(start - min(base, *total))
			local, body := sliceTextBody(string(raw), relative, summaryLimit)
			start = min(base, *total) + int64(local)
			next := start + int64(len(body))
			a.Summary = &body
			a.SummaryBytes = total
			a.SummaryOffset = &start
			if next < *total {
				a.SummaryNextOffset = &next
			}
		}
		doc.Annotations = append(doc.Annotations, a)
	}
	return rows.Err()
}
