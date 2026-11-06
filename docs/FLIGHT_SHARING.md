# Share flights with Plano

## Private Friends sharing

Both installations use the same Plano server. Under **Trip tools → Friends**, the sender creates an invitation with the name they want the recipient to see. Share the code with the intended recipient. The recipient uses it within 24 hours and chooses their own display name.

The connection shares no flights initially. The sender enables each flight separately. The recipient can see the shared flight's current provider status. The share ends if the sender untracks the flight or revokes its grant, or if either participant removes the connection. Sharing in the other direction uses a separate invitation.

The Friends response carries flight information and display names. It leaves seats, bookings, device data, and local receiver tracks in their own installation scopes. A revoked share does not affect ordinary flight search for public provider status.

### Alerts for shared flights

Under **Shared with you**, choose the alert level for each friend. Alerts start off. **Arrivals** sends landing and gate arrival updates. **All updates** also sends flight status changes such as gate changes, delays, takeoff, and baggage. Enable Plano notifications on the device to receive alerts. [Push setup](SELF_HOSTING.md#enable-live-data-and-push)

Alerts apply to new events on flights your friend has shared. A shared flight stays out of your Passport unless you track it yourself. Plano queues one alert per device and event even if you already track the flight or multiple friends share it. A queued alert keeps each permission it had when it was created. Untracking removes its personal permission. Changing your preference or revoking a share removes that share’s permission. Plano checks the remaining permissions after preparing provider credentials and before submitting the alert. Alerts already submitted to a push provider can still arrive.

## Public pickup links

On a tracked flight, tap **Share live link** to generate a 72-hour link. The pickup page distinguishes scheduled, estimated, and actual gate arrival. A diversion asks the viewer to check the pickup airport with the airline. It refreshes while the flight is active.

When the server has recent cached provider positions, the page maps the reported path and timestamps the latest position. Gaps remain visible when reports are more than five minutes apart. The self-hosted server supplies the map and keeps local receiver reports, seat assignments, and bookings private. A page can still show arrival details while the track is unavailable. Operators should use pickup links within the distribution terms of their FlightAware agreement. [AeroAPI terms by tier](https://www.flightaware.com/commercial/aeroapi/)

Anyone holding the link can open it without a Plano account. Use **Manage pickup links** to revoke one link. Untracking the flight removes its public links. A Friends grant is independent of any public pickup link.
