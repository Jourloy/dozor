// Stand-in for `next/link`. The Sidebar of @jourloy/00 imports it only as the default of `linkComponent`;
// the menu always passes its own link (see ../index.tsx), so this plain anchor is never actually rendered.
import type {AnchorHTMLAttributes} from "react";

export default function Link(props: AnchorHTMLAttributes<HTMLAnchorElement> & {href: string}) {
  return <a {...props} />;
}
