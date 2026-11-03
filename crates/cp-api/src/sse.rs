#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SseEvent {
    pub id: Option<String>,
    pub data: String,
}

pub struct SseParser {
    buffer: Vec<u8>,
    data: Vec<String>,
    has_data: bool,
    last_event_id: Option<String>,
    first_line: bool,
}

impl Default for SseParser {
    fn default() -> Self {
        Self {
            buffer: Vec::new(),
            data: Vec::new(),
            has_data: false,
            last_event_id: None,
            first_line: true,
        }
    }
}

impl SseParser {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn push(&mut self, chunk: &[u8]) -> Vec<SseEvent> {
        self.buffer.extend_from_slice(chunk);
        let mut events = Vec::new();

        while let Some((line_end, delimiter_len)) = self.next_line_end() {
            let mut line = String::from_utf8_lossy(&self.buffer[..line_end]).into_owned();
            self.buffer.drain(..line_end + delimiter_len);
            if self.first_line {
                self.first_line = false;
                if line.starts_with('\u{feff}') {
                    line.drain(..'\u{feff}'.len_utf8());
                }
            }
            self.process_line(&line, &mut events);
        }

        events
    }

    fn next_line_end(&self) -> Option<(usize, usize)> {
        for (i, byte) in self.buffer.iter().enumerate() {
            match byte {
                b'\n' => return Some((i, 1)),
                b'\r' => {
                    if i + 1 == self.buffer.len() {
                        return None;
                    }
                    return Some((i, if self.buffer[i + 1] == b'\n' { 2 } else { 1 }));
                }
                _ => {}
            }
        }
        None
    }

    fn process_line(&mut self, line: &str, events: &mut Vec<SseEvent>) {
        if line.is_empty() {
            if self.has_data {
                events.push(SseEvent {
                    id: self.last_event_id.clone(),
                    data: self.data.join("\n"),
                });
            }
            self.data.clear();
            self.has_data = false;
            return;
        }
        if line.starts_with(':') {
            return;
        }

        let (field, value) = line
            .split_once(':')
            .map_or((line, ""), |(field, value)| (field, value));
        let value = value.strip_prefix(' ').unwrap_or(value);
        match field {
            "data" => {
                self.data.push(value.to_string());
                self.has_data = true;
            }
            "id" if !value.contains('\0') => {
                self.last_event_id = Some(value.to_string());
            }
            _ => {}
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_a_line_split_across_chunks() {
        let mut parser = SseParser::new();
        assert!(parser.push(b"da").is_empty());
        assert_eq!(
            parser.push(b"ta: one\n\n"),
            vec![SseEvent {
                id: None,
                data: "one".into(),
            }]
        );
    }

    #[test]
    fn parses_crlf_split_across_chunks() {
        let mut parser = SseParser::new();
        assert!(parser.push(b"data: first\r").is_empty());
        assert_eq!(
            parser.push(b"\n\r\n"),
            vec![SseEvent {
                id: None,
                data: "first".into(),
            }]
        );
    }

    #[test]
    fn joins_multiline_data() {
        let mut parser = SseParser::new();
        assert_eq!(
            parser.push(b"data: first\ndata: second\n\n"),
            vec![SseEvent {
                id: None,
                data: "first\nsecond".into(),
            }]
        );
    }

    #[test]
    fn ignores_comments_and_keeps_event_id() {
        let mut parser = SseParser::new();
        assert_eq!(
            parser.push(b": connected\nid: kept\ndata: hello\n\n"),
            vec![SseEvent {
                id: Some("kept".into()),
                data: "hello".into(),
            }]
        );
    }

    #[test]
    fn strips_utf8_bom_split_across_chunks() {
        let mut parser = SseParser::new();
        assert!(parser.push(b"\xef").is_empty());
        assert_eq!(
            parser.push(b"\xbb\xbfdata: hello\n\n"),
            vec![SseEvent {
                id: None,
                data: "hello".into(),
            }]
        );
    }
}
