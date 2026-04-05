# Reserves and Weight

**Reserves** is a per-asset measure of some amount of funds that a client
has proven to mesh that it owns. Mesh validates and reports certain reserves.

**Weight** is a per-asset measure of the cumulative amount of funds a client has
locked up in mesh activities. Mesh does not do any validation of weight. Weight
is entirely reported by and used by clients. Mesh acts only to aggregate and
report **moving. weight**.

There are two types of weight. **Standing weight** and **moving weight**.
**Standing weight** is **weight** dedicated to standing orders or other types of
publicly visible funded activities. **Moving weight** is reported to and
aggregated by mesh and reported along with certain funded messages, such as
order broadcasts.

## Example weight accounting

As an example of how **reserves** and **weight** is used, imagine you are a
trader who wants to trade their 20 DCR for BTC. Before placing the order, you
would validate 20 DCR in **reserves** with mesh. The order itself is placed as
part of a **funded broadcast**, so the along with the broadcast, the mesh will
report your validated **reserves** and your **moving weight**.

Trader Z on the other side sees my order come with an attestation of my
**reserves** and my **moving weight**. They see that the I have 20 DCR in
**reserves** and 0 DCR **moving weight**. They scan their markets and see that I
have no other **standing weight** on the markets that they are monitoring. The
sum of my **standing** and **moving weight**, including the proposed order, is
then 20 DCR. My **reserves** of 20 DCR covers the **weight**, so they accept the
order.

Trader Z decides to offer me 1 BTC for 10 of my DCR. I do three things. 1) I
accept the match offer, 2) I update my standing order to only be for 10 DCR, and
3) I **check out** 1 BTC **moving weight** on Trader Z from mesh. 

Trader Z **checks out** 10 DCR **moving weight** on me. Add that to the
**standing weight** of 10 DCR in my updated order, and my **weight** is still 20
DCR. I sell any more DCR unless I reserve more funds. Trader Z keeps the 10 DCR
**checked out** until my swap tx reaches the requisite number of confirmations.
when they **release** it.


