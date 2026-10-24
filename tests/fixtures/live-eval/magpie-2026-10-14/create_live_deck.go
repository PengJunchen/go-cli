package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const oldStatus = "Release status: INITIAL"
const releaseStatus = "Release status: REVISED" // target: live_review_3A6527E73296.pptx
const mime = "application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"
const pns = "http://schemas.openxmlformats.org/presentationml/2006/main"
const ans = "http://schemas.openxmlformats.org/drawingml/2006/main"
const rns = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
const disclosure = "Synthetic QA inputs only - not product measurements"

var titles = []string{"LIVE_REVIEW_3A6527E73296", "What Was Checked", "Tool Cycle and Retained Facts", "Release Status"}
var facts = []string{"Synthetic checks completed: 53 of 100", "Synthetic median tool cycle: 3.1 seconds", "Synthetic review facts retained: 7 of 10"}

func esc(s string) string { var b bytes.Buffer; xml.EscapeText(&b, []byte(s)); return b.String() }
func must(err error) {
	if err != nil {
		panic(err)
	}
}
func rels(items string) string {
	return `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` + items + `</Relationships>`
}
func rel(id, typ, target string) string {
	return `<Relationship Id="` + id + `" Type="` + rns + `/` + typ + `" Target="` + target + `"/>`
}

const group = `<p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="0" cy="0"/><a:chOff x="0" y="0"/><a:chExt cx="0" cy="0"/></a:xfrm></p:grpSpPr>`

func shape(id int, text string, y, height, size int, color string) string {
	return fmt.Sprintf(`<p:sp><p:nvSpPr><p:cNvPr id="%d" name="Text %d"/><p:cNvSpPr txBox="1"/><p:nvPr/></p:nvSpPr><p:spPr><a:xfrm><a:off x="640000" y="%d"/><a:ext cx="10900000" cy="%d"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom><a:noFill/><a:ln><a:noFill/></a:ln></p:spPr><p:txBody><a:bodyPr wrap="square"/><a:lstStyle/><a:p><a:r><a:rPr lang="en-US" sz="%d"><a:solidFill><a:srgbClr val="%s"/></a:solidFill><a:latin typeface="Arial"/></a:rPr><a:t>%s</a:t></a:r><a:endParaRPr lang="en-US"/></a:p></p:txBody></p:sp>`, id, id, y, height, size, color, esc(text))
}
func slide(title string, lines []string) string {
	s := `<p:sld xmlns:p="` + pns + `" xmlns:a="` + ans + `" xmlns:r="` + rns + `"><p:cSld><p:bg><p:bgPr><a:solidFill><a:srgbClr val="F5F7FA"/></a:solidFill><a:effectLst/></p:bgPr></p:bg><p:spTree>` + group + shape(2, title, 500000, 950000, 3200, "142D4E")
	for i, line := range lines {
		s += shape(i+3, line, 1800000+i*850000, 750000, 2400, "243B53")
	}
	s += shape(len(lines)+3, disclosure, 6000000, 450000, 1400, "526579")
	return s + `</p:spTree></p:cSld><p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr></p:sld>`
}
func create(target string) {
	entries := map[string]string{}
	ct := `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/ppt/presentation.xml" ContentType="` + mime + `"/><Override PartName="/ppt/slideMasters/slideMaster1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slideMaster+xml"/><Override PartName="/ppt/slideLayouts/slideLayout1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slideLayout+xml"/><Override PartName="/ppt/theme/theme1.xml" ContentType="application/vnd.openxmlformats-officedocument.theme+xml"/>`
	pres := `<p:presentation xmlns:p="` + pns + `" xmlns:r="` + rns + `"><p:sldMasterIdLst><p:sldMasterId id="2147483648" r:id="rId5"/></p:sldMasterIdLst><p:sldIdLst>`
	pr := ""
	bodies := [][]string{{"Synthetic QA briefing"}, {facts[0]}, {facts[1], facts[2]}, {releaseStatus}}
	for i := 1; i <= 4; i++ {
		name := fmt.Sprintf("ppt/slides/slide%d.xml", i)
		entries[name] = slide(titles[i-1], bodies[i-1])
		entries[fmt.Sprintf("ppt/slides/_rels/slide%d.xml.rels", i)] = rels(rel("rId1", "slideLayout", "../slideLayouts/slideLayout1.xml"))
		ct += fmt.Sprintf(`<Override PartName="/%s" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slide+xml"/>`, name)
		pres += fmt.Sprintf(`<p:sldId id="%d" r:id="rId%d"/>`, 255+i, i)
		pr += rel(fmt.Sprintf("rId%d", i), "slide", fmt.Sprintf("slides/slide%d.xml", i))
	}
	entries["[Content_Types].xml"] = ct + `</Types>`
	entries["_rels/.rels"] = rels(rel("rId1", "officeDocument", "ppt/presentation.xml"))
	entries["ppt/presentation.xml"] = pres + `</p:sldIdLst><p:sldSz cx="12192000" cy="6858000"/><p:notesSz cx="6858000" cy="9144000"/></p:presentation>`
	entries["ppt/_rels/presentation.xml.rels"] = rels(pr + rel("rId5", "slideMaster", "slideMasters/slideMaster1.xml"))
	clr := `<p:clrMap accent1="accent1" accent2="accent2" accent3="accent3" accent4="accent4" accent5="accent5" accent6="accent6" bg1="lt1" bg2="lt2" folHlink="folHlink" hlink="hlink" tx1="dk1" tx2="dk2"/>`
	entries["ppt/slideMasters/slideMaster1.xml"] = `<p:sldMaster xmlns:p="` + pns + `" xmlns:a="` + ans + `" xmlns:r="` + rns + `"><p:cSld><p:spTree>` + group + `</p:spTree></p:cSld>` + clr + `<p:sldLayoutIdLst><p:sldLayoutId id="2147483649" r:id="rId1"/></p:sldLayoutIdLst><p:txStyles><p:titleStyle/><p:bodyStyle/><p:otherStyle/></p:txStyles></p:sldMaster>`
	entries["ppt/slideMasters/_rels/slideMaster1.xml.rels"] = rels(rel("rId1", "slideLayout", "../slideLayouts/slideLayout1.xml") + rel("rId2", "theme", "../theme/theme1.xml"))
	entries["ppt/slideLayouts/slideLayout1.xml"] = `<p:sldLayout xmlns:p="` + pns + `" xmlns:a="` + ans + `" type="blank" preserve="1"><p:cSld name="Blank"><p:spTree>` + group + `</p:spTree></p:cSld><p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr></p:sldLayout>`
	entries["ppt/slideLayouts/_rels/slideLayout1.xml.rels"] = rels(rel("rId1", "slideMaster", "../slideMasters/slideMaster1.xml"))
	theme := `<a:theme xmlns:a="` + ans + `" name="Plain"><a:themeElements><a:clrScheme name="Plain">`
	for i, n := range []string{"dk1", "lt1", "dk2", "lt2", "accent1", "accent2", "accent3", "accent4", "accent5", "accent6", "hlink", "folHlink"} {
		colors := []string{"142D4E", "FFFFFF", "243B53", "F5F7FA", "146C94", "287271", "735D78", "8B5E34", "526579", "427AA1", "0000FF", "800080"}
		theme += `<a:` + n + `><a:srgbClr val="` + colors[i] + `"/></a:` + n + `>`
	}
	theme += `</a:clrScheme><a:fontScheme name="Arial"><a:majorFont><a:latin typeface="Arial"/><a:ea typeface=""/><a:cs typeface=""/></a:majorFont><a:minorFont><a:latin typeface="Arial"/><a:ea typeface=""/><a:cs typeface=""/></a:minorFont></a:fontScheme><a:fmtScheme name="Plain"><a:fillStyleLst>`
	for i := 0; i < 3; i++ {
		theme += `<a:solidFill><a:schemeClr val="phClr"/></a:solidFill>`
	}
	theme += `</a:fillStyleLst><a:lnStyleLst>`
	for i := 0; i < 3; i++ {
		theme += `<a:ln w="9525"><a:solidFill><a:schemeClr val="phClr"/></a:solidFill><a:prstDash val="solid"/></a:ln>`
	}
	theme += `</a:lnStyleLst><a:effectStyleLst><a:effectStyle><a:effectLst/></a:effectStyle><a:effectStyle><a:effectLst/></a:effectStyle><a:effectStyle><a:effectLst/></a:effectStyle></a:effectStyleLst><a:bgFillStyleLst>`
	for i := 0; i < 3; i++ {
		theme += `<a:solidFill><a:schemeClr val="phClr"/></a:solidFill>`
	}
	entries["ppt/theme/theme1.xml"] = theme + `</a:bgFillStyleLst></a:fmtScheme></a:themeElements></a:theme>`
	f, err := os.Create(target)
	must(err)
	z := zip.NewWriter(f)
	for name, data := range entries {
		w, err := z.Create(name)
		must(err)
		_, err = w.Write([]byte(data))
		must(err)
	}
	must(z.Close())
	must(f.Close())
}
func update(target string) {
	z, err := zip.OpenReader(target)
	must(err)
	var changed []byte
	for _, f := range z.File {
		if f.Name == "ppt/slides/slide4.xml" {
			r, e := f.Open()
			must(e)
			data, e := io.ReadAll(r)
			must(e)
			must(r.Close())
			if !bytes.Contains(data, []byte(esc(oldStatus))) {
				panic("slide 4 old status missing")
			}
			changed = bytes.Replace(data, []byte(esc(oldStatus)), []byte(esc(releaseStatus)), 1)
		}
	}
	if changed == nil {
		panic("slide 4 missing")
	}
	temp, err := os.CreateTemp(filepath.Dir(target), ".live-deck-*.pptx")
	must(err)
	defer os.Remove(temp.Name())
	out := zip.NewWriter(temp)
	for _, f := range z.File {
		if f.Name == "ppt/slides/slide4.xml" {
			h := f.FileHeader
			w, e := out.CreateHeader(&h)
			must(e)
			_, e = w.Write(changed)
			must(e)
		} else {
			must(out.Copy(f))
		}
	}
	must(out.Close())
	must(temp.Close())
	must(z.Close())
	must(os.Rename(temp.Name(), target))
}
func verify(target string) {
	z, err := zip.OpenReader(target)
	must(err)
	defer z.Close()
	parts := map[string][]byte{}
	for _, f := range z.File {
		r, e := f.Open()
		must(e)
		b, e := io.ReadAll(r)
		must(e)
		must(r.Close())
		parts[f.Name] = b
		if strings.HasSuffix(f.Name, ".xml") || strings.HasSuffix(f.Name, ".rels") {
			d := xml.NewDecoder(bytes.NewReader(b))
			for {
				_, e = d.Token()
				if e == io.EOF {
					break
				}
				must(e)
			}
		}
	}
	var ct struct {
		Overrides []struct {
			Name string `xml:"PartName,attr"`
			Type string `xml:"ContentType,attr"`
		} `xml:"Override"`
	}
	must(xml.Unmarshal(parts["[Content_Types].xml"], &ct))
	found := false
	for _, o := range ct.Overrides {
		if o.Name == "/ppt/presentation.xml" && o.Type == mime {
			found = true
		}
	}
	if !found {
		panic("incorrect presentation MIME")
	}
	var pres struct {
		IDs []struct {
			ID string `xml:"id,attr"`
		} `xml:"sldIdLst>sldId"`
	}
	must(xml.Unmarshal(parts["ppt/presentation.xml"], &pres))
	if len(pres.IDs) != 4 {
		panic("presentation must contain four slides")
	}
	all := ""
	count := 0
	for name, b := range parts {
		if strings.HasPrefix(name, "ppt/slides/slide") && strings.HasSuffix(name, ".xml") {
			count++
		}
		_ = b
	}
	if count != 4 {
		panic("incorrect slide part count")
	}
	for i, title := range titles {
		b := parts[fmt.Sprintf("ppt/slides/slide%d.xml", i+1)]
		d := xml.NewDecoder(bytes.NewReader(b))
		texts := []string{}
		shapes := 0
		for {
			t, e := d.Token()
			if e == io.EOF {
				break
			}
			must(e)
			if s, ok := t.(xml.StartElement); ok {
				if s.Name.Space == pns && s.Name.Local == "sp" {
					shapes++
				}
				if s.Name.Space == ans && s.Name.Local == "t" {
					var text string
					must(d.DecodeElement(&text, &s))
					texts = append(texts, text)
				}
			}
		}
		if shapes < 3 || len(texts) == 0 || texts[0] != title {
			panic("missing editable title or text shapes")
		}
		joined := strings.Join(texts, "\n")
		if !strings.Contains(joined, disclosure) {
			panic("disclosure missing")
		}
		if i == 3 && !strings.Contains(joined, releaseStatus) {
			panic("release status missing")
		}
		all += joined + "\n"
	}
	for _, fact := range facts {
		if !strings.Contains(all, fact) {
			panic("required metric missing: " + fact)
		}
	}
	abs, err := filepath.Abs(target)
	must(err)
	fmt.Printf("Verified saved file: %s\nSlide count: %d\nPresentation MIME: %s\nAll required titles, synthetic facts, disclosure, status, and editable text shapes verified.\n", abs, count, mime)
}
func main() {
	if len(os.Args) != 2 {
		panic("usage: create_live_deck.go target.pptx")
	}
	target := os.Args[1]
	_, err := os.Stat(target)
	if os.IsNotExist(err) {
		create(target)
	} else {
		must(err)
		update(target)
	}
	verify(target)
}
