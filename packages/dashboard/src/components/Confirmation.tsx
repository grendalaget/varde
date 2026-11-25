import { AlertDialog, Button as HeroButton } from "@heroui/react";
import {
  createContext,
  useContext,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { Button, Field, inputClass } from "./ui";

type Confirmation = {
  title: string;
  action: string;
  detail?: string;
  expected?: string;
};

const ConfirmContext = createContext<
  (options: Confirmation) => Promise<boolean>
>(async () => false);

export function useConfirmation() {
  return useContext(ConfirmContext);
}

export function ConfirmationProvider({ children }: { children: ReactNode }) {
  const request = useRef<((answer: boolean) => void) | null>(null);
  const [options, setOptions] = useState<Confirmation | null>(null);
  const [open, setOpen] = useState(false);
  const [answer, setAnswer] = useState("");

  function close(accepted: boolean) {
    const resolve = request.current;
    request.current = null;
    setOpen(false);
    resolve?.(accepted);
  }

  function confirm(options: Confirmation) {
    if (request.current) return Promise.resolve(false);
    return new Promise<boolean>((resolve) => {
      request.current = resolve;
      setOptions(options);
      setAnswer("");
      setOpen(true);
    });
  }

  const enabled =
    options?.expected === undefined || answer === options.expected;

  return (
    <ConfirmContext.Provider value={confirm}>
      {children}
      {options && (
        <AlertDialog.Backdrop
          isOpen={open}
          isKeyboardDismissDisabled={false}
          onOpenChange={(open) => {
            if (!open) close(false);
          }}
        >
          <AlertDialog.Container placement="center">
            <AlertDialog.Dialog className="vd-confirm-dialog">
              <form
                className="flex min-h-0 flex-1 flex-col"
                onSubmit={(event) => {
                  event.preventDefault();
                  if (enabled) close(true);
                }}
              >
                <AlertDialog.Header className="shrink-0">
                  <AlertDialog.Heading>{options.title}</AlertDialog.Heading>
                </AlertDialog.Header>
                {(options.detail || options.expected !== undefined) && (
                  <AlertDialog.Body className="space-y-3 text-sm text-muted">
                    {options.detail && <p>{options.detail}</p>}
                    {options.expected !== undefined && (
                      <Field label={`Type ${options.expected} to confirm`}>
                        <input
                          autoFocus
                          className={inputClass}
                          autoComplete="off"
                          value={answer}
                          onChange={(event) => setAnswer(event.target.value)}
                        />
                      </Field>
                    )}
                  </AlertDialog.Body>
                )}
                <AlertDialog.Footer className="shrink-0">
                  <HeroButton
                    autoFocus={options.expected === undefined}
                    type="button"
                    variant="secondary"
                    onPress={() => close(false)}
                  >
                    Cancel
                  </HeroButton>
                  <Button type="submit" variant="danger" disabled={!enabled}>
                    {options.action}
                  </Button>
                </AlertDialog.Footer>
              </form>
            </AlertDialog.Dialog>
          </AlertDialog.Container>
        </AlertDialog.Backdrop>
      )}
    </ConfirmContext.Provider>
  );
}
