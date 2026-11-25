import { expect, test } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";
import { ConfigInput } from "./src/components/ConfigInput";
import { ControlNumber } from "./src/components/ControlNumber";

test("config choices use a named list box trigger and display the selected option", () => {
  const html = renderToStaticMarkup(
    <ConfigInput
      f={{
        name: "mode",
        type: "select",
        options: ["", "arms_race"],
        required: false,
        secret: false,
      }}
      value="arms_race"
      onChange={() => {}}
    />,
  );
  expect(html).toContain('aria-haspopup="listbox"');
  expect(html).toContain('aria-expanded="false"');
  expect(html).toContain("Mode");
  expect(html).toContain("Arms race");
});

test("number fields retain native validation and expose branded step controls", () => {
  const html = renderToStaticMarkup(
    <ControlNumber
      label="Copies"
      help="Safe copies"
      min={1}
      required
      value=""
      onChange={() => {}}
    />,
  );
  expect(html).toContain('type="number"');
  expect(html).toContain('min="1"');
  expect(html).toContain('required=""');
  expect(html).toContain("Safe copies");
  expect(html).toContain('aria-label="Decrease Copies"');
  expect(html).toContain('aria-label="Increase Copies"');
});
