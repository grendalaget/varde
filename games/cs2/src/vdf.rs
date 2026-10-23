use game_driver_api::Result;

#[derive(Clone, Debug)]
pub(crate) enum Value {
    Text(String),
    Object(Vec<(String, Value)>),
}

impl Value {
    pub(crate) fn get_mut(&mut self, key: &str) -> Option<&mut Self> {
        match self {
            Self::Object(entries) => entries
                .iter_mut()
                .find(|(k, _)| k.eq_ignore_ascii_case(key))
                .map(|(_, v)| v),
            _ => None,
        }
    }

    pub(crate) fn render(&self) -> String {
        fn quoted(s: &str) -> String {
            format!("\"{}\"", s.replace('\\', "\\\\").replace('"', "\\\""))
        }
        match self {
            Self::Text(s) => quoted(s),
            Self::Object(entries) => entries
                .iter()
                .map(|(key, value)| match value {
                    Self::Text(_) => format!("{} {}\n", quoted(key), value.render()),
                    Self::Object(_) => format!("{}\n{{\n{}}}\n", quoted(key), value.render()),
                })
                .collect(),
        }
    }

    pub(crate) fn get(&self, key: &str) -> Option<&Self> {
        self.entries()?
            .iter()
            .find(|(k, _)| k.eq_ignore_ascii_case(key))
            .map(|(_, v)| v)
    }

    pub(crate) fn entries(&self) -> Option<&[(String, Self)]> {
        match self {
            Self::Object(entries) => Some(entries),
            _ => None,
        }
    }

    pub(crate) fn text(&self) -> Option<&str> {
        match self {
            Self::Text(text) => Some(text),
            _ => None,
        }
    }
}

pub(crate) fn parse(input: &str) -> Result<Value> {
    if input.len() > 1_048_576 {
        return Err("Steam VDF exceeds 1 MiB".into());
    }
    let mut chars = input.chars().peekable();
    let mut tokens = Vec::new();
    while let Some(c) = chars.next() {
        match c {
            c if c.is_whitespace() => (),
            '/' if chars.peek() == Some(&'/') => {
                for c in chars.by_ref() {
                    if c == '\n' {
                        break;
                    }
                }
            }
            '{' | '}' => tokens.push(c.to_string()),
            '"' => {
                let mut token = String::new();
                loop {
                    match chars.next() {
                        Some('"') => break,
                        Some('\\') => match chars.next() {
                            Some(c @ ('\\' | '"')) => token.push(c),
                            Some(c) => {
                                token.push('\\');
                                token.push(c);
                            }
                            None => return Err("unterminated VDF escape".into()),
                        },
                        Some(c) => token.push(c),
                        None => return Err("unterminated VDF string".into()),
                    }
                }
                tokens.push(token);
            }
            c => {
                let mut token = c.to_string();
                while let Some(c) = chars.peek().copied() {
                    if c.is_whitespace() || matches!(c, '{' | '}') {
                        break;
                    }
                    token.push(chars.next().unwrap());
                }
                tokens.push(token);
            }
        }
    }
    fn object(tokens: &[String], i: &mut usize, depth: usize) -> Result<Value> {
        if depth > 64 {
            return Err("Steam VDF nesting exceeds 64".into());
        }
        let mut entries = Vec::new();
        while let Some(key) = tokens.get(*i) {
            if key == "}" {
                if depth == 0 {
                    return Err("unexpected VDF brace".into());
                }
                *i += 1;
                return Ok(Value::Object(entries));
            }
            if key == "{" {
                return Err("missing VDF key".into());
            }
            *i += 1;
            let value = tokens.get(*i).ok_or("missing VDF value")?;
            *i += 1;
            let value = match value.as_str() {
                "{" => object(tokens, i, depth + 1)?,
                "}" => return Err("missing VDF value".into()),
                _ => Value::Text(value.clone()),
            };
            entries.push((key.clone(), value));
        }
        if depth != 0 {
            return Err("unclosed VDF object".into());
        }
        Ok(Value::Object(entries))
    }
    object(&tokens, &mut 0, 0)
}
