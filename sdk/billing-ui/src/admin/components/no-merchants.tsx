import { AdminMessage } from "#orck/admin/ui-context"
import { useAdminContext } from "../context"
export function NoMerchants() {
  const { props } = useAdminContext()
  return (
    <div role="status" className="p-8">
      {props.emptyState ?? (
        <>
          <p>
            <AdminMessage id={"Select a merchant to view its billing."} />
          </p>
          {props.newMerchantHref && (
            <a href={props.newMerchantHref}>
              <AdminMessage id={"New merchant"} />
            </a>
          )}
        </>
      )}
    </div>
  )
}
