package node

import (
	"strings"
	"encoding/json"
	"strconv"
	"github.com/eluleci/lightning/message"
	"github.com/eluleci/lightning/adapter"
	"github.com/eluleci/lightning/util"
	"github.com/eluleci/lightning/config"
)

type Hub struct {
	res               string
	model             map[string]interface{}
	children          map[string]Hub
	subscribers       map[chan message.Message]chan message.Subscription
	Inbox             chan message.RequestWrapper
	parentInbox       chan message.RequestWrapper
	adapter           adapter.Adapter
}

func (h *Hub) Run() {
	defer func() {
		util.Log("debug", h.res+":  Stopped running.")

	}()

	util.Log("debug", h.res+":  Started running.")

	if len(h.subscribers) > 0 {
		util.Log("debug", h.res+": Hub has initial subscribers #"+strconv.Itoa(len(h.subscribers)))
	}

	for {
		select {
		case requestWrapper := <-h.Inbox:

			messageString, _ := json.Marshal(requestWrapper.Message)
			util.Log("debug", h.res+": Received message: "+string(messageString))

			if requestWrapper.Res == h.res {
				// if the resource of the message is this hub's resource

				// if there is a subscription channel inside the request, subscribe the request sender
				// we need to subscribe the channel before we continue because there may be children hub creation
				// afterwords and we need to give all subscriptions of this hub to it's children
				h.addSubscription(requestWrapper)

				if strings.EqualFold(requestWrapper.Message.Command, "get") {

					if config.SystemConfig.PersistItemInMemory && h.model != nil {
						// if persisting in memory and if the model exists, it means we already fetched data before.
						// so return the model to listener

						response := createResponse(requestWrapper.Message.Rid, h.res, 200, h.model, "")
						h.checkAndSend(requestWrapper.Listener, response)

					} else if config.SystemConfig.PersistListInMemory && len(h.children) > 0 {
						// if persisting lists in memory and if there are children hubs, it means we have the data
						// already. so directly collect the item data from hubs and return it back
						h.returnChildListToRequest(requestWrapper)

					} else if h.adapter != nil {
						// if there is no model, and if there is adapter, get the
						// data from the adapter first.
						h.executeGetOnAdapter(requestWrapper)

					} else {
						response := createResponse(requestWrapper.Message.Rid, h.res, 501, nil, "No adapter is set for ThunderDock Server.")
						h.checkAndSend(requestWrapper.Listener, response)
					}

				} else if strings.EqualFold(requestWrapper.Message.Command, "put") {

					if h.adapter != nil {
						// if there is adapter, execute the request from adapter directly
						h.executePutOnAdapter(requestWrapper)
					} else {
						response := createResponse(requestWrapper.Message.Rid, h.res, 501, nil, "No adapter is set for ThunderDock Server.")
						h.checkAndSend(requestWrapper.Listener, response)
					}

				}  else if strings.EqualFold(requestWrapper.Message.Command, "post") {

					if h.adapter != nil {
						// it is an object creation message under this domain
						h.executePostOnAdapter(requestWrapper)
					} else {
						response := createResponse(requestWrapper.Message.Rid, h.res, 501, nil, "No adapter is set for ThunderDock Server.")
						h.checkAndSend(requestWrapper.Listener, response)
					}

				}  else if strings.EqualFold(requestWrapper.Message.Command, "delete") {

					if h.adapter != nil {
						// it is an object deletion message under this domain
						if h.executeDeleteOnAdapter(requestWrapper) {

							// removing all subscribers and notifying them that they are removed from subscriptions
							for listenerChannel, _ := range h.subscribers {
								h.removeSubscription(listenerChannel, true)
							}

							// if deletion is successful, break the loop (destroy self)
							break
						}
					} else {
						response := createResponse(requestWrapper.Message.Rid, h.res, 501, nil, "No adapter is set for ThunderDock Server.")
						h.checkAndSend(requestWrapper.Listener, response)
					}

				} else if strings.EqualFold(requestWrapper.Message.Command, "::subscribe") {

					h.addSubscription(requestWrapper)

					response := createResponse(requestWrapper.Message.Rid, h.res, 200, nil, "")
					h.checkAndSend(requestWrapper.Listener, response)

				} else if strings.EqualFold(requestWrapper.Message.Command, "::unsubscribe") {
					// removing listener from subscriptions, no need to notify the listener that it is un-subscribed
					h.removeSubscription(requestWrapper.Listener, false)

					response := createResponse(requestWrapper.Message.Rid, h.res, 200, nil, "")
					h.checkAndSend(requestWrapper.Listener, response)

					if h.checkAndDestroy() {
						// if checkAndDestroy returns true, it means we're destroying. so break the for loop to destroy
						break
					}

				} else if strings.EqualFold(requestWrapper.Message.Command, "::deleteChild") {
					// this is a message from child hub for its' deletion. when a parent hub receives this message, it
					// means that the child hub is deleted explicitly.

					childRes := requestWrapper.Message.Body["::res"].(string)
					if _, exists := h.children[childRes]; exists {

						// send broadcast message of the object deletion
						requestWrapper.Message.Command = "delete"
						requestWrapper.Message.Res = h.res
						//						go func() {
						//							h.broadcast <- requestWrapper
						//						}()
						h.broadcastMessage(requestWrapper)

						// delete the child hub
						delete(h.children, childRes)
						util.Log("debug", h.res+": Deleted child "+string(childRes))

						if h.checkAndDestroy() {
							// if checkAndDestroy returns true, it means we're destroying. so break the for loop to destroy
							break
						}
					}
				} else if strings.EqualFold(requestWrapper.Message.Command, "::destroyChild") {

					childRes := requestWrapper.Message.Body["::res"].(string)
					if _, exists := h.children[childRes]; exists {

						// delete the child hub
						delete(h.children, childRes)
						util.Log("debug", h.res+": Destroyed child "+string(childRes))

						if h.checkAndDestroy() {
							// if checkAndDestroy returns true, it means we're destroying. so break the for loop to destroy
							break
						}
					}
				} else {
					var answer message.Message
					answer.Rid = requestWrapper.Message.Rid
					answer.Res = h.res
					answer.Status = 500
					answer.Body = h.model
					h.checkAndSend(requestWrapper.Listener, answer)
				}

			} else {
				// if the resource belongs to a children hub
				childRes := getChildRes(requestWrapper.Res, h.res)

				hub, exists := h.children[childRes]
				if !exists {
					//   if children doesn't exists, create children hub for the res
					hub = CreateHub(childRes, nil, h.Inbox)
					go hub.Run()
					h.children[childRes] = hub
				}
				//   forward message to the children hub
				hub.Inbox <- requestWrapper
			}
		}
	}
}

func (h *Hub) broadcastMessage(requestWrapper message.RequestWrapper) {

	util.Log("debug", h.res+": Broadcasting message. Number of subscribers: #"+strconv.Itoa(len(h.subscribers)))

	// removing unnecessary parts of the message if exists
	requestWrapper.Message.Rid = 0
	requestWrapper.Message.Headers = nil
	requestWrapper.Message.Status = 0

	// broadcasting a message to all connections. only the owner of the request doesn't receive this broadcast
	// because we send 'response message' to the request owner
	for listenerChannel, _ := range h.subscribers {
		if listenerChannel != requestWrapper.Listener {
			go h.checkAndSend(listenerChannel, requestWrapper.Message)
		}
	}
}

func (h *Hub) executeGetOnAdapter(requestWrapper message.RequestWrapper) {

	var answer message.Message
	answer.Rid = requestWrapper.Message.Rid
	answer.Res = h.res

	object, objectArray, requestErr := h.adapter.ExecuteGetRequest(requestWrapper)
	if requestErr != nil {
		util.Log("error", h.res+"Error occured when getting data from adapter. ")
		answer.Status = requestErr.Code
		answer.Body = requestErr.Body

	} else if object != nil {
		// if object is not null, it means that this is the object that this hub is responsible of
		util.Log("debug", h.res+": Received one object from adapter with id "+object[config.SystemConfig.ObjectIdentifier].(string))

		// adding a new field to object body for subscription purposes
		object["::res"] = h.res

		answer.Status = 200
		answer.Body = object

		// creating model holder if PersistInMemory enabled
		if config.SystemConfig.PersistItemInMemory {
			h.initialiseModel(object)
		}

	} else if objectArray != nil {
		// if object array is not null, it means that this hub is responsible of the collections of
		// of these objects. so we create a new hub for each object in the list and return the
		// result to listener
		util.Log("debug", h.res+": Received list of objects from adapter. Length: "+strconv.Itoa(len(objectArray)))

		// creating a new child hub and  adding it to children hub list
		for _, objectData := range (objectArray) {

			// generating res of the object: parentRes/objectId
			childRes := h.res + "/" + objectData[config.SystemConfig.ObjectIdentifier].(string)
			objectData["::res"] = childRes

			if existingChild, exists := h.children[childRes]; !exists {
				childHub := h.generateChild(childRes, objectData)
				h.children[childHub.res] = childHub
			} else {
				// adding the listener to child
				existingChild.addSubscription(requestWrapper)
				// TODO decide to give the fresh data to child hub or not
				util.Log("debug", h.res+": Child already exists for res "+childRes)
			}
		}

		answer.Status = 200
		answer.Body = make(map[string]interface{})
		answer.Body["::list"] = objectArray
	} else {
		util.Log("debug", h.res+": Receive object or list from adapter failed.")
		answer.Status = 500
	}

	// sending result of GET message
	h.checkAndSend(requestWrapper.Listener, answer)
}

func (h *Hub) executePutOnAdapter(requestWrapper message.RequestWrapper) {

	var answer message.Message
	answer.Rid = requestWrapper.Message.Rid
	answer.Res = h.res

	response, requestErr := h.adapter.ExecutePutRequest(requestWrapper)
	if requestErr != nil {
		util.Log("error", h.res+"Error occured when updating data via adapter. ")
		answer.Status = requestErr.Code
		answer.Body = requestErr.Body

	} else if response != nil {

		answer.Status = 200
		answer.Body = response

		// TODO: update the model holder if exists
		if h.model != nil {
			if response["updatedAt"] != nil {
				h.model["updatedAt"] = response["updatedAt"]
			}
			for k, v := range requestWrapper.Message.Body {
				h.model[k] = v
			}
		}

		requestWrapper.Message.Body["updatedAt"] = response["updatedAt"]
		//		go func() {
		//			h.broadcast <- requestWrapper
		//		}()
		h.broadcastMessage(requestWrapper)

	} else {
		answer.Status = 404
	}

	// sending result of GET message
	h.checkAndSend(requestWrapper.Listener, answer)
}

func (h *Hub) executePostOnAdapter(requestWrapper message.RequestWrapper) {

	var answer message.Message
	answer.Rid = requestWrapper.Message.Rid
	answer.Res = h.res

	response, requestErr := h.adapter.ExecutePostRequest(requestWrapper)
	if requestErr != nil {
		util.Log("error", h.res+"Error occured when posting data to adapter. ")
		answer.Status = requestErr.Code
		answer.Body = requestErr.Body

	} else if response != nil {

		objectData := requestWrapper.Message.Body

		// adding a new field 'res' to object body for subscription purposes
		objectRes := h.res + "/" + response[config.SystemConfig.ObjectIdentifier].(string)
		objectData["::res"] = objectRes
		objectData["createdAt"] = response["createdAt"]
		response["::res"] = objectRes

		answer.Status = 200
		answer.Res = objectRes
		answer.Body = response

		// generating new child hub for newly created object
		childHub := h.generateChild(objectRes, objectData)
		h.children[childHub.res] = childHub

		requestWrapper.Message.Rid = 0
		requestWrapper.Message.Body = objectData
		//		go func() {
		//			h.broadcast <- requestWrapper
		//		}()
		h.broadcastMessage(requestWrapper)

	} else {
		answer.Status = 500
	}

	// sending result of GET message
	h.checkAndSend(requestWrapper.Listener, answer)
}

func (h *Hub) executeDeleteOnAdapter(requestWrapper message.RequestWrapper) (isDeleted bool) {

	var answer message.Message
	answer.Rid = requestWrapper.Message.Rid
	answer.Res = h.res

	_, requestErr := h.adapter.ExecuteDeleteRequest(requestWrapper)
	if requestErr != nil {
		util.Log("error", h.res+"Error occured when deleting data via adapter. ")
		answer.Status = requestErr.Code
		answer.Body = requestErr.Body
	} else {
		// if there is no error, it means that the object is deleted successfully
		answer.Status = 200
		isDeleted = true

		// send broadcast message of the object deletion
		requestWrapper.Message.Rid = 0
		h.broadcastMessage(requestWrapper)

		var deleteRequest message.RequestWrapper
		deleteRequest.Message.Command = "::deleteChild"
		deleteRequest.Res = getParentRes(h.res)
		deleteRequest.Message.Body = make(map[string]interface{})
		deleteRequest.Message.Body["::res"] = h.res
		deleteRequest.Listener = requestWrapper.Listener       // for not sending push message from parent to connection
		h.parentInbox <- deleteRequest
	}

	// sending result of DELETE message
	h.checkAndSend(requestWrapper.Listener, answer)
	return
}

func (h *Hub) initialiseModel(data map[string]interface{}) {
	h.model = data
}

func (h *Hub) generateChild(objectRes string, objectData map[string]interface{}) Hub {

	// copying subscribers of parent to pass to the newly created child hub
	subscribersCopy := make(map[chan message.Message]chan message.Subscription)
	for listenChannel, subscriptionChannel := range (h.subscribers) {
		subscribersCopy[listenChannel] = subscriptionChannel
	}

	// creating a child hub with initial subscribers
	hub := CreateHub(objectRes, subscribersCopy, h.Inbox)
	go hub.Run()
	util.Log("debug", h.res+": Created a new child for res: "+hub.res+", with subscribers #"+strconv.Itoa(len(h.subscribers)))

	// saving model if PersistItemInMemory enabled
	if config.SystemConfig.PersistItemInMemory {
		hub.initialiseModel(objectData)
	}
	return hub
}

func (h *Hub) addSubscription(requestWrapper message.RequestWrapper) {
	defer func() {
		if r := recover(); r != nil {
			// the subscribe channel may be closed. catching the panic
		}
	}()

	if requestWrapper.Subscribe == nil {
		return
	}

	// add the connection if it is not already in subscribers list
	if _, exists := h.subscribers[requestWrapper.Listener]; !exists {
		var subscription message.Subscription
		subscription.Res = h.res
		subscription.InboxChannel = h.Inbox
		//		subscription.UnsubscriptionChannel = h.unsubscribe

		requestWrapper.Subscribe <- subscription
		h.subscribers[requestWrapper.Listener] = requestWrapper.Subscribe
		util.Log("debug", h.res+": Added new listener to subscribers. New size: #"+strconv.Itoa(len(h.subscribers)))
	}
}

func (h *Hub) removeSubscription(listenerChannel chan message.Message, notifyListener bool) {
	defer func() {
		if r := recover(); r != nil {
			// the subscribe channel may be closed. catching the panic
		}
	}()

	// remove the connection if it is already in subscribers list
	if subscribeChannel, exists := h.subscribers[listenerChannel]; exists {
		subscription := message.Subscription{h.res, nil}
		delete(h.subscribers, listenerChannel)

		if notifyListener {
			// notifying listener that it is removed from subscribers
			subscribeChannel <- subscription
		}
		util.Log("debug", h.res+": Removed a listener from subscribers. New size: #"+strconv.Itoa(len(h.subscribers)))
	}
}

func (h *Hub) checkAndSend(c chan message.Message, m message.Message) {
	defer func() {
		if r := recover(); r != nil {
			util.Log("debug", h.res+"Trying to send on closed channel. Removing channel from subscribers.")
			//			h.unsubscribe <- c
		}
	}()
	c <- m
}

func (h *Hub) returnChildListToRequest(requestWrapper message.RequestWrapper) {

	list := make([]map[string]interface{}, len(h.children))
	callback := make(chan message.Message)        // callback channel to get responses from children

	var getMessage message.Message
	getMessage.Command = "get"
	var rw message.RequestWrapper
	rw.Message = getMessage
	rw.Listener = callback

	// sending get messages to all children
	for k, childHub := range h.children {
		rw.Res = k
		childHub.addSubscription(requestWrapper)
		childHub.Inbox <- rw
	}

	// receiving responses (receiving response is done after sending all messages for preventing being blocked by a child)
	for i := 0; i < len(h.children); i++ {
		response := <-callback
		//						fmt.Println(response.Body)
		list[i] = response.Body
	}
	var answer message.Message
	answer.Rid = requestWrapper.Message.Rid
	answer.Res = h.res
	answer.Status = 200
	answer.Body = make(map[string]interface{})
	answer.Body["::list"] = list
	requestWrapper.Listener <- answer
}

func (h *Hub) checkAndDestroy() bool {

	if len(h.subscribers) == 0 && len(h.children) == 0 && config.SystemConfig.CleanupOnSubscriptionsOver {

		if h.res == "/" {
			// don't remove the root hub
			return false
		}
		util.Log("debug", h.res+": No more subscriber or child remained. Destroying...")

		// sending a message to parent to notify that this children is destroying itself
		var destroyRequest message.RequestWrapper
		destroyRequest.Res = getParentRes(h.res)
		destroyRequest.Message.Body = make(map[string]interface{})
		destroyRequest.Message.Body["::res"] = h.res
		destroyRequest.Message.Command = "::destroyChild"
		h.parentInbox <- destroyRequest
		return true
	}
	return false
}

func CreateHub(res string, initialSubscribers map[chan message.Message]chan message.Subscription, parentInbox chan message.RequestWrapper) (h Hub) {
	h.res = res
	h.children = make(map[string]Hub)
	h.Inbox = make(chan message.RequestWrapper)
	h.parentInbox = parentInbox
	h.adapter = adapter.RestAdapter{}

	if initialSubscribers != nil {
		h.subscribers = initialSubscribers

		// notifying connections that they are subscribed to a new hub
		for _, subscriptionChannel := range (initialSubscribers) {
			subscription := message.Subscription {h.res, h.Inbox}
			subscriptionChannel <- subscription
		}
	} else {
		h.subscribers = make(map[chan message.Message]chan message.Subscription, 0)
	}
	return
}

func createInitialiseRequest(objectData map[string]interface{}, objectRes string) message.RequestWrapper {

	var initialiseMessage message.Message
	initialiseMessage.Command = "::initialise"
	initialiseMessage.Body = objectData

	var initialiseRequest message.RequestWrapper
	initialiseRequest.Res = objectRes
	initialiseRequest.Message = initialiseMessage
	return initialiseRequest
}

func getChildRes(res, parentRes string) (fullPath string) {
	res = strings.Trim(res, "/")
	parentRes = strings.Trim(parentRes, "/")
	currentResSize := len(parentRes)
	resSuffix := res[currentResSize:]
	trimmedSuffix := strings.Trim(resSuffix, "/")
	directChild := strings.Split(trimmedSuffix, "/")
	relativePath := directChild[0]
	if len(parentRes) > 0 {
		fullPath = "/"+parentRes+"/"+relativePath
	} else {
		fullPath = "/"+relativePath
	}
	return
}

func getParentRes(res string) (path string) {
	res = strings.Trim(res, "/")
	li := strings.LastIndex(res, "/")
	if li == -1 {
		// if there is no "/" char in trimmed version of res, it means that the parent is root
		return "/"
	}
	path = "/"+res[:li]
	return
}

func createResponse(rid int, res string, status int, body map[string]interface{}, errorMessage string) (response message.Message) {
	response.Rid = rid
	response.Res = res
	response.Status = status
	if body == nil && len(errorMessage) > 0 {
		body = make(map[string]interface{})
		body["error"] = errorMessage
	}
	response.Body = body
	return
}
