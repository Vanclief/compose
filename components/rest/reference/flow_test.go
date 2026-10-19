package reference

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const flowSpec = `
openapi: 3.0.3
info:
  title: Flow
  version: "1"
paths:
  /stores/{store_id}/pets:
    parameters:
      - {name: store_id, in: path, required: true, schema: {type: integer}}
    get:
      operationId: pets.list
      parameters:
        - {name: limit, in: query, schema: {type: integer}}
        - {name: owner_id, in: query, schema: {type: string}}
      responses:
        "200":
          description: OK
          content:
            application/json:
              schema: {$ref: '#/components/schemas/PetList'}
    post:
      operationId: pets.create
      requestBody:
        content:
          application/json:
            schema: {$ref: '#/components/schemas/NewPet'}
      responses:
        "201":
          description: Created
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Pet'}
        default:
          description: Error
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Error'}
  /stores/{store_id}/pets/{id}:
    get:
      operationId: pets.get
      parameters:
        - {name: store_id, in: path, required: true, schema: {type: integer}}
        - {name: id, in: path, required: true, schema: {type: integer, format: int64}}
      responses:
        "200":
          description: OK
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Pet'}
  /stores:
    get:
      operationId: stores.list
      responses:
        "200":
          description: OK
          content:
            application/json:
              schema:
                type: array
                items: {$ref: '#/components/schemas/StoreWithStats'}
  /orders:
    post:
      operationId: orders.create
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              properties:
                pet_id: {type: integer}
                default_store_id: {type: integer}
                note: {type: string}
      responses:
        "204":
          description: Done
  /health:
    get:
      operationId: health
      responses:
        "204":
          description: Up
components:
  schemas:
    Pet:
      type: object
      properties:
        id: {type: integer}
        name: {type: string}
    NewPet:
      type: object
      properties:
        name: {type: string}
    PetList:
      type: object
      properties:
        pets:
          type: array
          items: {$ref: '#/components/schemas/Pet'}
        next: {type: string}
    Store:
      type: object
      properties:
        id: {type: integer}
    StoreWithStats:
      type: object
      properties:
        id: {type: integer}
        pets: {type: integer}
    Error:
      type: object
      properties:
        message: {type: string}
`

func flowPage(t *testing.T) (*pageView, map[string]*opView) {
	t.Helper()
	doc, err := Parse([]byte(flowSpec))
	require.NoError(t, err)
	p := buildPage(doc, Options{}, nil)
	ops := map[string]*opView{}
	for _, tag := range p.Tags {
		for _, ov := range tag.Ops {
			ops[ov.OperationID] = ov
		}
	}
	return p, ops
}

func port(t *testing.T, ports []portView, key string) portView {
	t.Helper()
	for _, p := range ports {
		if p.Key == key {
			return p
		}
	}
	require.Failf(t, "missing port", "%s", key)
	return portView{}
}

func TestFlowMachine(t *testing.T) {
	_, ops := flowPage(t)

	list := ops["pets.list"].Machine
	require.Equal(t, "pets", list.Resource)
	require.Len(t, list.Inputs, 2)
	store := port(t, list.Inputs, "in:path:store_id")
	require.True(t, store.Required)
	require.Equal(t, "num", store.Kind)
	opt := port(t, list.Inputs, "in:optional")
	require.Equal(t, "2 optional", opt.Name)
	require.Equal(t, []string{"limit", "owner_id"}, opt.Names)

	// A wrapper without an id of its own returns a list of its items.
	out := port(t, list.Outputs, "out:200")
	require.True(t, out.Main)
	require.True(t, out.Stack)
	require.Equal(t, "Pet[]", out.Type)

	create := ops["pets.create"].Machine
	body := port(t, create.Inputs, "in:body")
	require.Equal(t, "NewPet", body.Type)
	require.False(t, body.Required)
	created := port(t, create.Outputs, "out:201")
	require.Equal(t, "Pet", created.Type)
	require.False(t, created.Stack)
	errOut := port(t, create.Outputs, "out:default")
	require.False(t, errOut.Main)

	health := ops["health"].Machine
	require.Len(t, health.Inputs, 1)
	require.True(t, health.Inputs[0].Ghost)
	require.Equal(t, "empty", port(t, health.Outputs, "out:204").Type)
}

func TestFlowChains(t *testing.T) {
	p, ops := flowPage(t)

	// {id} under pets takes a Pet; the list is the first place to get one,
	// and the operation itself is never its own source.
	get := ops["pets.get"].Machine
	id := port(t, get.Inputs, "in:path:id")
	require.Same(t, ops["pets.list"], id.Source)
	require.Equal(t, 1, id.MoreSources)

	// stores lists StoreWithStats, which still produces the Store its
	// collection is named after.
	store := port(t, get.Inputs, "in:path:store_id")
	require.Same(t, ops["stores.list"], store.Source)

	// Body ids resolve too, dropping leading words until a model matches.
	order := port(t, ops["orders.create"].Machine.Inputs, "in:body")
	require.Len(t, order.Fields, 2)
	require.Equal(t, "pet_id", order.Fields[0].Name)
	require.Same(t, ops["pets.list"], order.Fields[0].Source)
	require.Equal(t, "default_store_id", order.Fields[1].Name)
	require.Same(t, ops["stores.list"], order.Fields[1].Source)

	// Pet is taken by pets.get and orders.create.
	require.Equal(t, 2, port(t, ops["pets.create"].Machine.Outputs, "out:201").Users)
	require.Equal(t, 2, port(t, ops["pets.list"].Machine.Outputs, "out:200").Users)

	var data flowData
	require.NoError(t, json.Unmarshal([]byte(p.Flow), &data))
	require.Len(t, data.Ops, len(ops))
	var pet flowModel
	for _, m := range data.Models {
		if m.Name == "Pet" {
			pet = m
		}
	}
	require.Equal(t, "model/Pet", pet.Anchor)
	require.Len(t, pet.By, 3)
	require.Len(t, pet.Use, 2)

	for _, m := range p.Models {
		if m.Name == "Pet" {
			require.Equal(t, 3, m.MadeBy)
			require.Equal(t, 2, m.UsedBy)
		}
	}
}

func TestFlowRender(t *testing.T) {
	doc, err := Parse([]byte(flowSpec))
	require.NoError(t, err)
	out, err := Render(doc, Options{})
	require.NoError(t, err)
	html := string(out)
	requireBalanced(t, html)

	require.Contains(t, html, `<script type="application/json" id="flow-data">{"ops":[`)
	get := section(t, html, `id="operation/pets-get"`)
	require.Contains(t, get, `<div class="mc-node"><span class="m m-get">GET</span><span class="mc-res">pets</span></div>`)
	require.Contains(t, get, `<span class="pin">id</span><span class="lchip">path</span></button><div class="src"><span class="pty">int64</span><span class="src-k">from</span><a href="#operation/pets-list">`)
	require.Contains(t, get, `<button type="button" class="pop-tg" data-pop="by">+1</button>`)
	require.Contains(t, get, `<div class="io" data-in="path">`)

	create := section(t, html, `id="operation/pets-create"`)
	require.Contains(t, create, `<span class="pin">NewPet</span><span class="lchip">body</span>`)

	list := section(t, html, `id="operation/pets-list"`)
	require.Contains(t, list, `class="port k-obj main stack" data-port="out:200"`)
	require.Contains(t, list, `<span class="pout">Pet<span class="arr">[]</span></span><span class="status schip s2xx">200</span>`)
	require.Contains(t, list, `used by 2 &rarr;`)

	model := section(t, html, `id="model/Pet"`)
	require.Contains(t, model, `&larr; made by 3`)
	require.True(t, strings.Contains(html, `class="nav-op" href="#operation/pets-get"`) && strings.Contains(html, ` data-m="m`))
}
