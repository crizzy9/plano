package feeds

import (
	"errors"
	"strings"
	"testing"
)

const aixmReaderIdentity = "<e:series>B</e:series><e:number>0123</e:number><e:year>2026</e:year><e:type>N</e:type>"

func aixmReaderMessage(base, notification, content string) string {
	return "<m:AIXMBasicMessage xmlns:m=\"" + base + "/message\" xmlns:e=\"" + base + "/event\">" +
		"<m:hasMember><e:Event><e:timeSlice><e:EventTimeSlice><e:" + notification + ">" + content +
		"</e:" + notification + "></e:EventTimeSlice></e:timeSlice></e:Event></m:hasMember></m:AIXMBasicMessage>"
}

func TestReadAIXMNOTAMsSupportedPaths(t *testing.T) {
	notice := "<e:NOTAM>" + aixmReaderIdentity +
		"<e:location> KSFO </e:location><e:text>RWY 10 &amp; 11 CLSD</e:text>" +
		"<e:publisher xmlns:xlink=\"" + xlinkNS + "\" xlink:href=\"urn:example:issuer\"/>" +
		"<e:translation><e:type>LOCAL_FORMAT</e:type></e:translation></e:NOTAM>"
	standalone := strings.Replace(notice, "<e:NOTAM>", "<e:NOTAM xmlns:e=\""+aixm511+"/event\">", 1)
	for name, input := range map[string]string{
		"5.1.1":      "<?xml version=\"1.0\"?>" + aixmReaderMessage(aixm511, "notification", notice),
		"5.1":        aixmReaderMessage(aixm51, "textNOTAM", strings.ReplaceAll(notice, "e:publisher", "e:publisherNOF")),
		"standalone": standalone,
	} {
		t.Run(name, func(t *testing.T) {
			notices, err := readAIXMNOTAMs(strings.NewReader(input))
			if err != nil || len(notices) != 1 {
				t.Fatalf("got %d notices, err %v", len(notices), err)
			}
			n := notices[0]
			if n.Series != "B" || n.Number != "0123" || n.Year != "2026" || n.Type != "N" || n.Location != "KSFO" ||
				n.Text != "RWY 10 & 11 CLSD" || n.Publisher != "urn:example:issuer" {
				t.Fatalf("unexpected notice %+v", n)
			}
		})
	}
}

func TestReadAIXMNOTAMsRejectsInvalidDocuments(t *testing.T) {
	element := func(content string) string { return "<e:NOTAM>" + content + "</e:NOTAM>" }
	wrap := func(content string) string { return aixmReaderMessage(aixm511, "notification", content) }
	notice := element(aixmReaderIdentity)
	for name, input := range map[string]string{
		"multiple roots":    wrap(notice) + wrap(notice),
		"trailing text":     wrap(notice) + "tail",
		"directive":         "<!DOCTYPE root>" + wrap(notice),
		"duplicate field":   wrap(element(aixmReaderIdentity + "<e:type>C</e:type>")),
		"nested scalar":     wrap(element(aixmReaderIdentity + "<e:text>RWY <e:other>CLSD</e:other></e:text>")),
		"foreign field":     wrap(element(aixmReaderIdentity + "<text>RWY CLSD</text>")),
		"unknown container": wrap("<e:unrelated>" + notice + "</e:unrelated>"),
		"missing identity":  wrap(element("<e:text>RWY CLSD</e:text>")),
		"malformed":         wrap(strings.TrimSuffix(notice, "</e:NOTAM>")),
		"depth limit":       wrap(element(aixmReaderIdentity + strings.Repeat("<x>", maxAIXMDepth) + strings.Repeat("</x>", maxAIXMDepth))),
		"notice limit":      wrap(strings.Repeat(notice, maxAIXMNOTAMs+1)),
		"byte limit":        wrap(notice) + strings.Repeat(" ", maxAIXMBytes),
	} {
		t.Run(name, func(t *testing.T) {
			notices, err := readAIXMNOTAMs(strings.NewReader(input))
			if !errors.Is(err, ErrInvalidQuery) || len(notices) != 0 {
				t.Fatalf("got %d notices, err %v", len(notices), err)
			}
		})
	}
}
